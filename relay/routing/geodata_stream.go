package routing

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxGeoGroupSize = 64 << 20
	maxGeoEntrySize = 1 << 20
	geoReadBuffer   = 32 << 10
)

// sectionField holds offsets, not a slice of the database. Even unknown fields
// and groups can be checked/skipped without allocating their advertised size.
type sectionField struct {
	number, kind int
	integer      uint64
	offset, size int64
}

func sectionFields(ctx context.Context, source io.ReaderAt, start, size int64, visit func(sectionField) error) error {
	section := io.NewSectionReader(source, start, size)
	input := bufio.NewReaderSize(section, geoReadBuffer)
	var consumed int64
	readVarint := func() (uint64, error) {
		var v uint64
		for i := 0; i < 10; i++ {
			b, err := input.ReadByte()
			if err != nil {
				return 0, io.ErrUnexpectedEOF
			}
			consumed++
			if i == 9 && b > 1 {
				return 0, errors.New("protobuf varint overflow")
			}
			v |= uint64(b&127) << uint(7*i)
			if b < 128 {
				return v, nil
			}
		}
		return 0, errors.New("protobuf varint overflow")
	}
	for consumed < size {
		if err := ctx.Err(); err != nil {
			return err
		}
		tag, err := readVarint()
		if err != nil {
			return err
		}
		f := sectionField{number: int(tag >> 3), kind: int(tag & 7)}
		if f.number <= 0 || tag>>3 > 0x1fffffff {
			return errors.New("invalid protobuf field")
		}
		switch f.kind {
		case 0:
			f.integer, err = readVarint()
		case 1:
			f.size = 8
		case 2:
			var n uint64
			n, err = readVarint()
			if err == nil {
				if n > uint64(size-consumed) {
					return io.ErrUnexpectedEOF
				}
				f.size = int64(n)
			}
		case 5:
			f.size = 4
		default:
			return errors.New("unsupported protobuf wire type")
		}
		if err != nil {
			return err
		}
		if f.size > size-consumed {
			return io.ErrUnexpectedEOF
		}
		f.offset = start + consumed
		if err = visit(f); err != nil {
			return err
		}
		// Seek the section rather than reading a giant unneeded field into memory.
		consumed += f.size
		if f.size > 0 {
			if f.size <= int64(input.Buffered()) {
				_, err = input.Discard(int(f.size))
				if err != nil {
					return err
				}
			} else {
				if _, err = section.Seek(consumed, io.SeekStart); err != nil {
					return err
				}
				input.Reset(section)
			}
		}
	}
	return nil
}

func readSection(source io.ReaderAt, f sectionField, max int64) ([]byte, error) {
	if f.size > max {
		return nil, fmt.Errorf("geo field exceeds %d byte limit", max)
	}
	b := make([]byte, int(f.size))
	_, err := source.ReadAt(b, f.offset)
	return b, err
}

// walkGeoGroups makes two bounded passes over each selected group so protobuf
// field order is irrelevant (the code may legally follow the entries).
func walkGeoGroups(ctx context.Context, source io.ReaderAt, size int64, needed map[string]bool, begin func(string, bool) error, entry func(string, []byte) error) error {
	if size <= 0 || size > MaxGeoSize {
		return fmt.Errorf("geo database missing or exceeds %d MiB", MaxGeoSize>>20)
	}
	seen := make(map[string]bool)
	err := sectionFields(ctx, source, 0, size, func(top sectionField) error {
		if top.number != 1 {
			return nil
		}
		if top.kind != 2 {
			return errors.New("invalid geo list group")
		}
		if top.size > maxGeoGroupSize {
			return fmt.Errorf("geo group exceeds %d MiB", maxGeoGroupSize>>20)
		}
		code := ""
		inverse := false
		entries := 0
		err := sectionFields(ctx, source, top.offset, top.size, func(f sectionField) error {
			switch f.number {
			case 1:
				if f.kind != 2 {
					return errors.New("invalid geo group code")
				}
				b, err := readSection(source, f, 128)
				if err != nil {
					return err
				}
				code = strings.ToLower(string(b))
			case 2:
				if f.kind != 2 {
					return fmt.Errorf("geo entry %d has invalid wire type %d", entries+1, f.kind)
				}
				if f.size > maxGeoEntrySize {
					return fmt.Errorf("geo entry %d size %d bytes exceeds %d byte limit", entries+1, f.size, maxGeoEntrySize)
				}
				// This pass scans every group without retaining its entries. Only
				// the second, selected-group pass needs an entry-count budget.
				entries++
			case 3:
				if f.kind != 0 {
					return errors.New("invalid geo inverse field")
				}
				inverse = f.integer != 0
			}
			return nil
		})
		if err != nil {
			return err
		}
		if code == "" {
			return errors.New("missing geo group code")
		}
		if !needed[code] {
			return nil
		}
		if seen[code] {
			return fmt.Errorf("duplicate geo group %q", code)
		}
		seen[code] = true
		if err = begin(code, inverse); err != nil {
			return err
		}
		return sectionFields(ctx, source, top.offset, top.size, func(f sectionField) error {
			if f.number != 2 {
				return nil
			}
			b, err := readSection(source, f, maxGeoEntrySize)
			if err != nil {
				return err
			}
			return entry(code, b)
		})
	})
	if err != nil {
		return err
	}
	for code := range needed {
		if !seen[code] {
			return fmt.Errorf("missing geo group %q", code)
		}
	}
	return nil
}

func geoIPReader(ctx context.Context, r io.ReaderAt, size int64, needed map[string]bool) (map[string][]netip.Prefix, error) {
	out := make(map[string][]netip.Prefix)
	count := 0
	err := walkGeoGroups(ctx, r, size, needed, func(code string, inverse bool) error {
		if inverse {
			return errors.New("inverseMatch inside GeoIP database is not supported")
		}
		out[code] = nil
		return nil
	}, func(code string, b []byte) error {
		count++
		if count > 250000 {
			return fmt.Errorf("selected GeoIP group %q exceeds mobile entry limit (250000 total selected entries)", code)
		}
		p, err := parseGeoIPEntry(b)
		if err == nil {
			out[code] = append(out[code], p)
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("invalid GeoIP database: %w", err)
	}
	for code := range needed {
		if len(out[code]) == 0 {
			return nil, fmt.Errorf("missing or empty geoip group %q", code)
		}
	}
	return out, nil
}
func geoSiteReader(ctx context.Context, r io.ReaderAt, size int64, needed map[string]bool) (map[string][]geoDomain, error) {
	out := make(map[string][]geoDomain)
	count, attrs := 0, 0
	err := walkGeoGroups(ctx, r, size, needed, func(code string, _ bool) error { out[code] = nil; return nil }, func(code string, b []byte) error {
		count++
		if count > 250000 {
			return fmt.Errorf("selected GeoSite group %q exceeds mobile entry limit (250000 total selected entries)", code)
		}
		d, err := parseGeoSiteEntry(b, &attrs)
		if err == nil {
			out[code] = append(out[code], d)
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("invalid GeoSite database: %w", err)
	}
	for code := range needed {
		if len(out[code]) == 0 {
			return nil, fmt.Errorf("missing or empty geosite group %q", code)
		}
	}
	return out, nil
}
func loadGeoIP(data []byte, needed map[string]bool) (map[string][]netip.Prefix, error) {
	return geoIPReader(context.Background(), bytes.NewReader(data), int64(len(data)), needed)
}
func loadGeoSite(data []byte, needed map[string]bool) (map[string][]geoDomain, error) {
	return geoSiteReader(context.Background(), bytes.NewReader(data), int64(len(data)), needed)
}

func compileFiles(ctx context.Context, p *Profile, dir string) (*Matcher, error) {
	needIPs, needSites := neededGeoGroups(p)
	ips, sites := map[string][]netip.Prefix{}, map[string][]geoDomain{}
	readFile := func(name string, parse func(io.ReaderAt, int64) error) error {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		return parse(f, info.Size())
	}
	if len(needIPs) > 0 {
		err := readFile("geoip.dat", func(r io.ReaderAt, n int64) error {
			var err error
			ips, err = geoIPReader(ctx, r, n, needIPs)
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	if len(needSites) > 0 {
		err := readFile("geosite.dat", func(r io.ReaderAt, n int64) error {
			var err error
			sites, err = geoSiteReader(ctx, r, n, needSites)
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return compileGroups(p, ips, sites)
}
