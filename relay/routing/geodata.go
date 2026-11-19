package routing

import (
	"encoding/binary"
	"errors"
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

func parseGeoIPEntry(entry []byte) (netip.Prefix, error) {
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
		return netip.Prefix{}, errors.New("invalid GeoIP CIDR")
	}
	if addr.Is4In6() {
		if bits < 96 {
			return netip.Prefix{}, errors.New("ambiguous IPv4-mapped GeoIP prefix")
		}
		addr = addr.Unmap()
		bits -= 96
	}
	return netip.PrefixFrom(addr, int(bits)).Masked(), nil
}

func parseGeoSiteEntry(entry []byte, selectedAttributes *int) (geoDomain, error) {
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
			(*selectedAttributes)++
			if *selectedAttributes > 500000 {
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
		return geoDomain{}, err
	}
	if _, err := newDomainRule(d.kind, d.value); err != nil {
		return geoDomain{}, err
	}
	return d, nil
}
