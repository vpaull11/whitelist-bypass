package routing

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
)

type field struct {
	number  int
	kind    int
	bytes   []byte
	integer uint64
}

func fields(data []byte, visit func(field) error) error {
	for len(data) > 0 {
		tag, n := binary.Uvarint(data)
		if n <= 0 {
			return errors.New("invalid protobuf tag")
		}
		data = data[n:]
		f := field{number: int(tag >> 3), kind: int(tag & 7)}
		if f.number == 0 {
			return errors.New("invalid protobuf field")
		}
		switch f.kind {
		case 0:
			v, n := binary.Uvarint(data)
			if n <= 0 {
				return errors.New("invalid protobuf varint")
			}
			f.integer = v
			data = data[n:]
		case 1:
			if len(data) < 8 {
				return io.ErrUnexpectedEOF
			}
			data = data[8:]
		case 2:
			size, n := binary.Uvarint(data)
			if n <= 0 || size > uint64(len(data)-n) {
				return errors.New("invalid protobuf length")
			}
			data = data[n:]
			f.bytes = data[:int(size)]
			data = data[int(size):]
		case 5:
			if len(data) < 4 {
				return io.ErrUnexpectedEOF
			}
			data = data[4:]
		default:
			return errors.New("unsupported protobuf wire type")
		}
		if err := visit(f); err != nil {
			return err
		}
	}
	return nil
}

type geoDomain struct {
	kind  uint64
	value string
	attrs map[string]bool
}

func group(data []byte) (string, [][]byte, bool, error) {
	var code string
	var entries [][]byte
	inverse := false
	err := fields(data, func(f field) error {
		switch f.number {
		case 1:
			if f.kind != 2 || len(f.bytes) > 128 {
				return errors.New("invalid geo group code")
			}
			code = strings.ToLower(string(f.bytes))
		case 2:
			if f.kind != 2 || len(entries) >= 500000 {
				return errors.New("invalid/oversized geo group")
			}
			entries = append(entries, f.bytes)
		case 3:
			if f.kind != 0 {
				return errors.New("invalid geo inverse field")
			}
			inverse = f.integer != 0
		}
		return nil
	})
	if code == "" {
		return "", nil, false, errors.New("missing geo group code")
	}
	return code, entries, inverse, err
}
func loadGeoIP(data []byte, needed map[string]bool) (map[string][]netip.Prefix, error) {
	if len(data) == 0 || len(data) > MaxGeoSize {
		return nil, errors.New("GeoIP database missing or exceeds 64 MiB")
	}
	out := map[string][]netip.Prefix{}
	selectedEntries := 0
	err := fields(data, func(f field) error {
		if f.number != 1 {
			return nil
		}
		if f.kind != 2 {
			return errors.New("invalid GeoIPList")
		}
		code, entries, inverse, err := group(f.bytes)
		if err != nil {
			return err
		}
		if !needed[code] {
			return nil
		}
		selectedEntries += len(entries)
		if selectedEntries > 250000 {
			return errors.New("selected GeoIP groups exceed mobile entry limit")
		}
		if inverse {
			return errors.New("inverseMatch inside GeoIP database is not supported")
		}
		if _, exists := out[code]; exists {
			return fmt.Errorf("duplicate geoip group %q", code)
		}
		out[code] = nil
		for _, entry := range entries {
			var ip []byte
			var bits uint64
			err := fields(entry, func(f field) error {
				switch f.number {
				case 1:
					if f.kind != 2 {
						return errors.New("invalid CIDR IP")
					}
					ip = f.bytes
				case 2:
					if f.kind != 0 {
						return errors.New("invalid CIDR prefix")
					}
					bits = f.integer
				}
				return nil
			})
			addr, ok := netip.AddrFromSlice(ip)
			if err != nil || !ok || bits > uint64(addr.BitLen()) {
				return errors.New("invalid GeoIP CIDR")
			}
			if addr.Is4In6() {
				if bits < 96 {
					return errors.New("ambiguous IPv4-mapped GeoIP prefix")
				}
				addr = addr.Unmap()
				bits -= 96
			}
			out[code] = append(out[code], netip.PrefixFrom(addr, int(bits)).Masked())
		}
		return nil
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
func loadGeoSite(data []byte, needed map[string]bool) (map[string][]geoDomain, error) {
	if len(data) == 0 || len(data) > MaxGeoSize {
		return nil, errors.New("GeoSite database missing or exceeds 64 MiB")
	}
	out := map[string][]geoDomain{}
	selectedEntries, selectedAttributes := 0, 0
	err := fields(data, func(f field) error {
		if f.number != 1 {
			return nil
		}
		if f.kind != 2 {
			return errors.New("invalid GeoSiteList")
		}
		code, entries, _, err := group(f.bytes)
		if err != nil {
			return err
		}
		if !needed[code] {
			return nil
		}
		selectedEntries += len(entries)
		if selectedEntries > 250000 {
			return errors.New("selected GeoSite groups exceed mobile entry limit")
		}
		if _, exists := out[code]; exists {
			return fmt.Errorf("duplicate geosite group %q", code)
		}
		out[code] = nil
		for _, entry := range entries {
			d := geoDomain{}
			err := fields(entry, func(f field) error {
				switch f.number {
				case 1:
					if f.kind != 0 {
						return errors.New("invalid domain type")
					}
					d.kind = f.integer
				case 2:
					if f.kind != 2 || len(f.bytes) > 4096 {
						return errors.New("invalid domain value")
					}
					d.value = string(f.bytes)
				case 3:
					if f.kind != 2 {
						return errors.New("invalid domain attribute")
					}
					key := ""
					enabled := false
					err := fields(f.bytes, func(a field) error {
						switch a.number {
						case 1:
							if a.kind != 2 || len(a.bytes) > 128 {
								return errors.New("invalid attribute key")
							}
							key = strings.ToLower(string(a.bytes))
						case 2, 3:
							if a.kind != 0 {
								return errors.New("invalid attribute value")
							}
							enabled = a.integer != 0
						}
						return nil
					})
					if err != nil {
						return err
					}
					if key == "" {
						return errors.New("empty attribute key")
					}
					selectedAttributes++
					if selectedAttributes > 500000 {
						return errors.New("selected GeoSite attributes exceed mobile limit")
					}
					if d.attrs == nil {
						d.attrs = make(map[string]bool)
					}
					if len(d.attrs) >= 16 {
						if _, exists := d.attrs[key]; !exists {
							return errors.New("too many attributes on a GeoSite domain")
						}
					}
					d.attrs[key] = enabled
				}
				return nil
			})
			if err != nil {
				return err
			}
			if _, err := newDomainRule(d.kind, d.value); err != nil {
				return err
			}
			out[code] = append(out[code], d)
		}
		return nil
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
