package routing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Protector must remove an fd from Android's VPN before connect/bind.
type Protector interface{ Protect(fd int) bool }

func SocketControl(p Protector) func(string, string, syscall.RawConn) error {
	return func(network, address string, raw syscall.RawConn) error {
		if p == nil {
			return errors.New("DIRECT unavailable: socket protector missing")
		}
		ok := false
		if err := raw.Control(func(fd uintptr) { ok = p.Protect(int(fd)) }); err != nil {
			return err
		}
		if !ok {
			return errors.New("DIRECT socket protection failed")
		}
		return nil
	}
}

type Progress interface {
	OnProgress(stage string, bytes int64)
	IsCancelled() bool
}
type Snapshot struct {
	Profile *Profile
	Matcher *Matcher
	ID      string
}

var storeMu sync.Mutex

func Preview(input string) (string, error) {
	p, err := Parse(input)
	if err != nil {
		return "", err
	}
	// Validate ordinary rules before requesting network access; missing geo groups
	// are validated after their databases have been downloaded.
	b, err := json.Marshal(p)
	return string(b), err
}
func readLimited(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err == nil && int64(len(b)) > max {
		return nil, errors.New("routing file exceeds size limit")
	}
	return b, err
}
func Load(root string) (*Snapshot, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	return load(root)
}
func load(root string) (*Snapshot, error) {
	pointer, err := readLimited(filepath.Join(root, "current"), 128)
	if err != nil {
		return nil, errors.New("no saved routing profile")
	}
	id := strings.TrimSpace(string(pointer))
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 16 {
		return nil, errors.New("invalid routing snapshot pointer")
	}
	dir := filepath.Join(root, id)
	b, err := readLimited(filepath.Join(dir, "profile.json"), MaxProfileSize)
	if err != nil {
		return nil, err
	}
	p, err := Parse(string(b))
	if err != nil {
		return nil, err
	}
	var ip, site []byte
	if p.NeedGeoIP() {
		ip, err = readLimited(filepath.Join(dir, "geoip.dat"), MaxGeoSize)
		if err != nil {
			return nil, err
		}
	}
	if p.NeedGeoSite() {
		site, err = readLimited(filepath.Join(dir, "geosite.dat"), MaxGeoSize)
		if err != nil {
			return nil, err
		}
	}
	m, err := Compile(p, ip, site)
	if err != nil {
		return nil, err
	}
	return &Snapshot{Profile: p, Matcher: m, ID: id}, nil
}

// Prepare creates and validates a snapshot in a new directory. Only the final
// pointer replacement publishes it; failures leave the old snapshot untouched.
func Prepare(root, input string, cb Progress, p Protector) (string, error) {
	profile, err := Parse(input)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	var idBytes [16]byte
	if _, err = rand.Read(idBytes[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(idBytes[:])
	dir := filepath.Join(root, id)
	if err = os.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(dir)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if cb != nil {
		go func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if cb.IsCancelled() {
						cancel()
						return
					}
				}
			}
		}()
	}
	client := downloadClient(p)
	defer client.CloseIdleConnections()
	for _, item := range []struct {
		name, url string
		needed    bool
	}{{"geoip.dat", profile.Geoipurl, profile.NeedGeoIP()}, {"geosite.dat", profile.Geositeurl, profile.NeedGeoSite()}} {
		if !item.needed {
			continue
		}
		if item.url == "" {
			return "", fmt.Errorf("%s URL is required", item.name)
		}
		if cb != nil {
			cb.OnProgress(item.name, 0)
		}
		if err = download(ctx, client, item.url, filepath.Join(dir, item.name), item.name, cb); err != nil {
			return "", err
		}
	}
	normalized, _ := json.Marshal(profile)
	if err = os.WriteFile(filepath.Join(dir, "profile.json"), normalized, 0600); err != nil {
		return "", err
	}
	var ip, site []byte
	if profile.NeedGeoIP() {
		ip, err = readLimited(filepath.Join(dir, "geoip.dat"), MaxGeoSize)
		if err != nil {
			return "", err
		}
	}
	if profile.NeedGeoSite() {
		site, err = readLimited(filepath.Join(dir, "geosite.dat"), MaxGeoSize)
		if err != nil {
			return "", err
		}
	}
	if _, err = Compile(profile, ip, site); err != nil {
		return "", err
	}
	if cb != nil && cb.IsCancelled() {
		return "", context.Canceled
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	// A second update may have published while this one was downloading. Its
	// current snapshot remains valid until this fully validated pointer commits.
	old, _ := os.ReadFile(filepath.Join(root, "current"))
	tmp, err := os.CreateTemp(root, "pointer-")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err = tmp.WriteString(id); err != nil {
		tmp.Close()
		return "", err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err = tmp.Close(); err != nil {
		return "", err
	}
	if err = os.Rename(tmpPath, filepath.Join(root, "current")); err != nil {
		return "", err
	}
	committed = true
	// Only delete the previous owned snapshot, never an arbitrary imported path.
	oldID := strings.TrimSpace(string(old))
	oldBytes, e := hex.DecodeString(oldID)
	if e == nil && len(oldBytes) == 16 && oldID != id {
		os.RemoveAll(filepath.Join(root, oldID))
	}
	return string(normalized), nil
}
func Remove(root string) error {
	storeMu.Lock()
	defer storeMu.Unlock()
	pointer, err := os.ReadFile(filepath.Join(root, "current"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	id := strings.TrimSpace(string(pointer))
	b, err := hex.DecodeString(id)
	if err != nil || len(b) != 16 {
		return errors.New("invalid snapshot pointer")
	}
	if err = os.Remove(filepath.Join(root, "current")); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(root, id))
}
func downloadClient(p Protector) *http.Client {
	dialer := net.Dialer{Timeout: 15 * time.Second}
	if p != nil {
		dialer.Control = SocketControl(p)
	}
	tr := &http.Transport{TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, errors.New("geodata host lookup failed")
		}
		for _, ip := range ips {
			if !publicIP(ip) {
				return nil, errors.New("geodata host resolves to a private/reserved address")
			}
		}
		for _, ip := range ips {
			c, e := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if e == nil {
				return c, nil
			}
		}
		return nil, errors.New("geodata connection failed")
	}
	return &http.Client{Transport: tr, Timeout: 3 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many geodata redirects")
		}
		_, err := ValidateDownloadURL(req.URL.String())
		return err
	}}
}
func download(ctx context.Context, client *http.Client, raw, path, stage string, cb Progress) error {
	if _, err := ValidateDownloadURL(raw); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return errors.New("invalid geodata request")
	}
	req.Header.Set("User-Agent", "RoutingClient/1")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s download failed (network/TLS/cancelled)", stage)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s download HTTP %d", stage, resp.StatusCode)
	}
	if resp.ContentLength > MaxGeoSize {
		return errors.New("geodata download exceeds 64 MiB")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, e := resp.Body.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > MaxGeoSize {
				return errors.New("geodata download exceeds 64 MiB")
			}
			if _, err = f.Write(buf[:n]); err != nil {
				return err
			}
			if cb != nil {
				cb.OnProgress(stage, total)
				if cb.IsCancelled() {
					return context.Canceled
				}
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return errors.New("geodata body read failed")
		}
	}
	if total == 0 {
		return errors.New("empty geodata download")
	}
	return f.Sync()
}
