package routing

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeLargeGeoFixture(t testing.TB, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Many legal unneeded groups with opaque bounded entries, followed by the
	// selected group. No huge allocation is required to construct the fixture.
	entry := wireBytes(2, bytes.Repeat([]byte{0}, 1<<20))
	body := append(wireBytes(1, []byte("unused")), entry...)
	prefix := binary.AppendUvarint([]byte{0x0a}, uint64(len(body)))
	for i := 0; i < 66; i++ {
		if _, err = f.Write(prefix); err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = f.Write(geoIPFixture("wanted")); err != nil {
		t.Fatal(err)
	}
}
func TestLargeGeodataFileImportAndOfflineLoad(t *testing.T) {
	source := filepath.Join(t.TempDir(), "large.dat")
	writeLargeGeoFixture(t, source)
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= 64<<20 {
		t.Fatal("fixture not over old cap")
	}
	root := t.TempDir()
	id := strings.Repeat("a", 32)
	dir := filepath.Join(root, id)
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// Exercise the real streamed downloader with a synthetic HTTP transport.
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		f, err := os.Open(source)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, ContentLength: info.Size(), Body: f}, nil
	})}
	if err = download(context.Background(), client, "https://example.com/geoip.dat", filepath.Join(dir, "geoip.dat"), "geoip.dat", nil); err != nil {
		t.Fatal(err)
	}
	p := testProfile()
	p.ProxyIP = []string{"geoip:wanted"}
	b, _ := json.Marshal(p)
	if err = os.WriteFile(filepath.Join(dir, "profile.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "current"), []byte(id), 0600); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	snapshot, err := Load(root)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Matcher.Decide("", []netip.Addr{netip.MustParseAddr("5.1.2.3")}) != Proxy {
		t.Fatal("large database rule not loaded")
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("file %.1f MiB, total parser/matcher allocations %.2f MiB", float64(info.Size())/(1<<20), float64(allocated)/(1<<20))
	if allocated > 16<<20 {
		t.Fatalf("parser allocated whole database: %d bytes", allocated)
	}
	// Failed replacement validation cannot modify the existing pointer.
	invalid := testProfile()
	invalid.ProxyIP = []string{"geoip:missing"}
	if _, err = compileFiles(context.Background(), invalid, dir); err == nil {
		t.Fatal("missing group accepted")
	}
	current, _ := os.ReadFile(filepath.Join(root, "current"))
	if string(current) != id {
		t.Fatal("current profile changed")
	}
}
func TestStreamingParsersMatchByteAPIAndUnorderedGroups(t *testing.T) {
	p := testProfile()
	p.ProxyIP = []string{"geoip:ru"}
	p.ProxySites = []string{"geosite:youtube@test"}
	ip, site := geoIPFixture("ru"), geoSiteFixture("youtube")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "geoip.dat"), ip, 0600)
	os.WriteFile(filepath.Join(dir, "geosite.dat"), site, 0600)
	m, err := Compile(p, ip, site)
	if err != nil {
		t.Fatal(err)
	}
	file, err := compileFiles(context.Background(), p, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, domain := range []string{"www.youtube.com", "unmatched.example"} {
		if m.Decide(domain, nil) != file.Decide(domain, nil) {
			t.Fatal("file and byte APIs disagree")
		}
	}
	// Legal protobuf code after entries, ignored fixed-width/varint fields.
	cidr := append(wireBytes(1, []byte{5, 1, 2, 0}), wireInt(2, 24)...)
	group := append(wireBytes(2, cidr), wireBytes(1, []byte("ru"))...)
	group = append(group, 0x21, 0, 0, 0, 0, 0, 0, 0, 0) // field4 fixed64
	group = append(group, 0x2d, 0, 0, 0, 0)             // field5 fixed32
	ip = wireBytes(1, group)
	parsed, err := loadGeoIP(ip, map[string]bool{"ru": true})
	if err != nil || len(parsed["ru"]) != 1 {
		t.Fatal("code-after-entries failed", err)
	}
}
func TestStreamingGeoLimitsAndCancellation(t *testing.T) {
	good := geoIPFixture("ru")
	for _, bad := range [][]byte{
		{0x0a, 0x80}, {0x0a, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
		{0x0a, 10, 1}, {0}, {0x0b},
		append(append([]byte{}, good...), good...),
	} {
		if _, err := loadGeoIP(bad, map[string]bool{"ru": true}); err == nil {
			t.Fatalf("bad protobuf accepted: %x", bad)
		}
	}
	oversized := binary.AppendUvarint([]byte{0x0a}, uint64(maxGeoGroupSize+1))
	// Sparse file supplies the advertised bytes without allocating them in Go.
	path := filepath.Join(t.TempDir(), "oversized.dat")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Write(oversized)
	f.Truncate(int64(len(oversized)) + maxGeoGroupSize + 1)
	if _, err = geoIPReader(context.Background(), f, int64(len(oversized))+maxGeoGroupSize+1, map[string]bool{"ru": true}); err == nil || !strings.Contains(err.Error(), "group exceeds") {
		t.Fatal("oversized group accepted", err)
	}
	if _, err = geoIPReader(context.Background(), f, MaxGeoSize+1, nil); err == nil {
		t.Fatal("oversized file accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = geoIPReader(ctx, bytes.NewReader(good), int64(len(good)), map[string]bool{"ru": true}); err == nil {
		t.Fatal("cancel ignored")
	}
}

type syntheticGeoBody struct{ remaining int64 }

func (r *syntheticGeoBody) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), r.remaining)
	clear(p[:int(n)])
	r.remaining -= n
	return int(n), nil
}
func (*syntheticGeoBody) Close() error { return nil }
func TestUnknownLengthDownloadStopsAtLimit(t *testing.T) {
	root := t.TempDir()
	// Use a body that emits bytes without allocating the entire 256 MiB.
	body := &syntheticGeoBody{remaining: MaxGeoSize + 1}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: -1, Body: body}, nil
	})}
	path := filepath.Join(root, "geoip.dat")
	err := download(context.Background(), client, "https://example.com/data", path, "geoip.dat", nil)
	if err == nil || !strings.Contains(err.Error(), "geoip.dat download exceeds 256 MiB") {
		t.Fatal("stream cap not enforced", err)
	}
	info, e := os.Stat(path)
	if e != nil {
		t.Fatal(e)
	}
	if info.Size() > MaxGeoSize {
		t.Fatal("wrote beyond cap")
	}
}

func BenchmarkStreamingLargeGeoIP(b *testing.B) {
	path := filepath.Join(b.TempDir(), "large.dat")
	writeLargeGeoFixture(b, path)
	f, err := os.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	b.ReportAllocs()
	b.SetBytes(info.Size())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := geoIPReader(context.Background(), f, info.Size(), map[string]bool{"wanted": true}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestPrepareLargeDatabaseRollbackAndCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.dat")
	writeLargeGeoFixture(t, path)
	info, _ := os.Stat(path)
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		f, err := os.Open(path)
		return &http.Response{StatusCode: 200, ContentLength: info.Size(), Body: f}, err
	})}
	root := t.TempDir()
	p := testProfile()
	p.ProxyIP = []string{"geoip:wanted"}
	p.Geoipurl = "https://example.com/geoip.dat"
	if _, err := prepareWithClient(root, profileJSON(p), nil, client); err != nil {
		t.Fatal(err)
	}
	before, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	p.ProxyIP = []string{"geoip:missing"}
	if _, err = prepareWithClient(root, profileJSON(p), nil, client); err == nil {
		t.Fatal("invalid replacement published")
	}
	after, err := Load(root)
	if err != nil || before.ID != after.ID {
		t.Fatal("failed update replaced working snapshot", err)
	}
	p.ProxyIP = []string{"geoip:wanted"}
	if _, err = prepareWithClient(root, profileJSON(p), cancelledProgress{}, client); err == nil {
		t.Fatal("cancelled replacement published")
	}
	after, err = Load(root)
	if err != nil || before.ID != after.ID {
		t.Fatal("cancelled update changed pointer", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatal("incomplete snapshots left on disk", len(entries))
	}
}

func TestExactFileLimitAndOversizedEntry(t *testing.T) {
	// A sparse unknown top-level field fills the file up to exactly the limit;
	// the parser must seek it, not allocate/read 256 MiB.
	selected := geoIPFixture("ru")
	unknownSize := MaxGeoSize - len(selected) - 5 // tag1 + four-byte length
	prefix := binary.AppendUvarint([]byte{0x12}, uint64(unknownSize))
	if len(prefix) != 5 {
		t.Fatal("bad boundary fixture")
	}
	path := filepath.Join(t.TempDir(), "boundary.dat")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Write(selected)
	f.Write(prefix)
	f.Truncate(MaxGeoSize)
	if _, err = geoIPReader(context.Background(), f, MaxGeoSize, map[string]bool{"ru": true}); err != nil {
		t.Fatal("exact file cap rejected", err)
	}
	// Entry size checked even for unneeded groups, before allocating entry bytes.
	group := append(wireBytes(1, []byte("unused")), binary.AppendUvarint([]byte{0x12}, maxGeoEntrySize+1)...)
	group = append(group, make([]byte, maxGeoEntrySize+1)...)
	bad := wireBytes(1, group)
	if _, err = loadGeoIP(bad, nil); err == nil {
		t.Fatal("oversized entry accepted")
	}
}

func TestUnusedHugeGroupDoesNotRejectSelectedRules(t *testing.T) {
	// More than the old 500000-entry scan limit. Code deliberately follows
	// entries: selection cannot rely on protobuf field ordering.
	entry := wireBytes(2, append(wireInt(1, 2), wireBytes(2, []byte("unused.example"))...))
	unused := bytes.Repeat(entry, 500001)
	unused = append(unused, wireBytes(1, []byte("ru-blocked-all"))...)
	data := append(wireBytes(1, unused), geoSiteFixture("youtube")...)
	got, err := loadGeoSite(data, map[string]bool{"youtube": true})
	if err != nil || len(got["youtube"]) != 1 {
		t.Fatal("unused large group rejected import", err)
	}
	if _, err = loadGeoSite(data, map[string]bool{"ru-blocked-all": true}); err == nil || !strings.Contains(err.Error(), `selected GeoSite group "ru-blocked-all" exceeds mobile entry limit`) {
		t.Fatal("selected-entry budget lost or ambiguous error", err)
	}
}

func TestGeoEntryErrorsDistinguishWireTypeAndSize(t *testing.T) {
	for _, tc := range []struct {
		entry []byte
		want  string
	}{
		{wireInt(2, 1), "invalid wire type"},
		{wireBytes(2, make([]byte, maxGeoEntrySize+1)), "byte limit"},
	} {
		group := append(wireBytes(1, []byte("unused")), tc.entry...)
		_, err := loadGeoSite(wireBytes(1, group), nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("expected %s, got %v", tc.want, err)
		}
	}
}

// Opt-in check against public database snapshots downloaded into isolated
// directories. Ordinary unit tests stay deterministic and network-free.
func TestExternalBundledProfileGeodata(t *testing.T) {
	ipPath, sitePath := os.Getenv("ROUTING_TEST_GEOIP"), os.Getenv("ROUTING_TEST_GEOSITE")
	if ipPath == "" || sitePath == "" {
		t.Skip("set ROUTING_TEST_GEOIP and ROUTING_TEST_GEOSITE to local database snapshots")
	}
	b, err := os.ReadFile("../../android-app/app/src/main/assets/default-routing.txt")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(string(b))
	if err != nil {
		t.Fatal(err)
	}
	needIP, needSite := neededGeoGroups(p)
	ip, err := os.Open(ipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ip.Close()
	ipInfo, err := ip.Stat()
	if err != nil {
		t.Fatal(err)
	}
	ips, err := geoIPReader(context.Background(), ip, ipInfo.Size(), needIP)
	if err != nil {
		t.Fatal(err)
	}
	site, err := os.Open(sitePath)
	if err != nil {
		t.Fatal(err)
	}
	defer site.Close()
	siteInfo, err := site.Stat()
	if err != nil {
		t.Fatal(err)
	}
	sites, err := geoSiteReader(context.Background(), site, siteInfo.Size(), needSite)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = compileGroups(p, ips, sites); err != nil {
		t.Fatal(err)
	}
	for code, entries := range ips {
		t.Logf("GeoIP %s: %d prefixes", code, len(entries))
	}
	for code, entries := range sites {
		t.Logf("GeoSite %s: %d domains", code, len(entries))
	}
	// Exercise the same atomic import and offline-reload path as Android, using
	// the downloaded snapshots as HTTP bodies (no further network requests).
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		path := ipPath
		if strings.HasSuffix(req.URL.Path, "geosite.dat") {
			path = sitePath
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		return &http.Response{StatusCode: 200, ContentLength: info.Size(), Body: f}, nil
	})}
	root := t.TempDir()
	if _, err = prepareWithClient(root, profileJSON(p), nil, client); err != nil {
		t.Fatal("real profile import", err)
	}
	if _, err = Load(root); err != nil {
		t.Fatal("real profile offline reload", err)
	}
	t.Log("Real bundled profile: atomic import and offline reload succeeded")
}
