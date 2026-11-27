package ui_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oleksandr-soldatov/uefimgr/internal/demo"
	"github.com/oleksandr-soldatov/uefimgr/internal/efi"
	"github.com/oleksandr-soldatov/uefimgr/internal/esp"
	"github.com/oleksandr-soldatov/uefimgr/internal/manager"
	"github.com/oleksandr-soldatov/uefimgr/internal/store"
	"github.com/oleksandr-soldatov/uefimgr/internal/target"
	"github.com/oleksandr-soldatov/uefimgr/internal/ui"
)

type fixture struct {
	t     *testing.T
	api   *ui.API
	store *store.FileStore
	dir   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	fs, err := store.OpenFile(filepath.Join(dir, "vars.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := demo.Seed(fs); err != nil {
		t.Fatal(err)
	}
	session := manager.NewSession(fs, filepath.Join(dir, "backups"))
	api := ui.NewAPI(session, esp.NewSimulated(), "test")
	api.Simulated = true
	return &fixture{t: t, api: api, store: fs, dir: dir}
}

type response struct {
	View    ui.View              `json:"view"`
	ID      uint16               `json:"id"`
	Name    string               `json:"name"`
	Applied manager.ApplyResult  `json:"applied"`
	Volumes []esp.Volume         `json:"volumes"`
	Entries []esp.DirEntry       `json:"entries"`
	Backups []manager.BackupInfo `json:"backups"`
	Path    string               `json:"path"`

	Editable   bool               `json:"editable"`
	Reason     string             `json:"reason"`
	Partitions []target.Partition `json:"partitions"`
}

// call goes through JSON both ways, like the page does.
func (f *fixture) call(method, params string) (response, error) {
	f.t.Helper()
	res, err := f.api.Call(method, json.RawMessage(params))
	if err != nil {
		return response{}, err
	}
	raw, err := json.Marshal(res)
	if err != nil {
		f.t.Fatal(err)
	}
	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		f.t.Fatalf("%s: %v in %s", method, err, raw)
	}
	return out, nil
}

func (f *fixture) ok(method, params string) response {
	f.t.Helper()
	res, err := f.call(method, params)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, params, err)
	}
	return res
}

func (f *fixture) fails(method, params, want string) {
	f.t.Helper()
	_, err := f.call(method, params)
	if err == nil || !strings.Contains(err.Error(), want) {
		f.t.Fatalf("%s %s: error %v, want one containing %q", method, params, err, want)
	}
}

func find(st *manager.State, id uint16) manager.Entry {
	e, _ := st.Find(id)
	return e
}

func TestDemoState(t *testing.T) {
	f := newFixture(t)
	v := f.ok("state", "").View
	if v.Version != "test" || !v.Simulated {
		t.Fatalf("view = %+v", v)
	}
	if !reflect.DeepEqual(v.Current.Order, []uint16{0, 1, 2, 3}) || len(v.Current.Unordered) != 1 {
		t.Fatalf("order = %v, unordered = %+v", v.Current.Order, v.Current.Unordered)
	}
	if len(v.Plan.Writes) != 0 || len(v.Plan.Risks) != 0 {
		t.Fatalf("a fresh session has nothing staged: %+v", v.Plan)
	}
	net := find(v.Current, 3)
	if net.PartitionGUID != "" || net.Broken {
		t.Fatalf("network entry = %+v", net)
	}
	if opts := f.ok("targets", `{"id":3}`); opts.Editable || len(opts.Partitions) != 0 || opts.Reason == "" {
		t.Fatalf("network entry targets = %+v", opts)
	}
	if !v.Current.FirmwareUISupported {
		t.Fatal("demo firmware supports the setup request")
	}
}

func TestEditsAreStagedThenApplied(t *testing.T) {
	f := newFixture(t)
	created := f.ok("create", `{"description":"rEFInd","path":"EFI/refind/refind_x64.efi","partition":"volume:sim-esp","first":true,"inactive":true}`)
	if created.Name != "Boot0004" {
		t.Fatalf("created %s", created.Name)
	}
	e := find(created.View.Draft, created.ID)
	if e.Active || e.FilePath != `\EFI\refind\refind_x64.efi` || e.OrderIndex != 0 {
		t.Fatalf("staged entry = %+v", e)
	}
	if _, ok := created.View.Current.Find(created.ID); ok {
		t.Fatal("the entry must not exist in firmware before apply")
	}

	f.ok("delete", `{"id":2}`)
	f.ok("setActive", `{"id":1,"active":false}`)
	f.ok("setNext", `{"id":1}`)
	f.ok("setTimeout", `{"seconds":10}`)
	f.ok("setFirmwareUI", `{"on":true}`)
	v := f.ok("setOrder", `{"ids":[4,1,0,3,7]}`).View

	var writes []string
	for _, w := range v.Plan.Writes {
		writes = append(writes, w.Op+" "+w.Name)
	}
	want := []string{
		"update Boot0001", "create Boot0004", "update BootOrder", "delete Boot0002",
		"create BootNext", "update Timeout", "create OsIndications",
	}
	if !reflect.DeepEqual(writes, want) {
		t.Fatalf("plan = %v\nwant  %v", writes, want)
	}
	if names, _ := f.store.Names(); len(names) != 9 {
		t.Fatalf("firmware changed before apply: %v", names)
	}

	applied := f.ok("apply", `{}`)
	if applied.Applied.Written != 7 || applied.Applied.Backup == "" {
		t.Fatalf("applied = %+v", applied.Applied)
	}
	cur := applied.View.Current
	if !reflect.DeepEqual(cur.Order, []uint16{4, 1, 0, 3, 7}) || len(cur.Unordered) != 0 {
		t.Fatalf("order = %v", cur.Order)
	}
	if cur.BootNext == nil || *cur.BootNext != 1 || *cur.Timeout != 10 || !cur.FirmwareUIRequested {
		t.Fatalf("state = %+v", cur)
	}
	if len(applied.View.Plan.Writes) != 0 {
		t.Fatal("plan not empty after apply")
	}
	if list := f.ok("backups", "").Backups; len(list) != 1 || list[0].Kind != "auto" || list[0].Entries != 5 {
		t.Fatalf("backups = %+v", list)
	}
}

func TestDiscard(t *testing.T) {
	f := newFixture(t)
	f.ok("delete", `{"id":1}`)
	v := f.ok("discard", "").View
	if len(v.Plan.Writes) != 0 || len(v.Draft.Entries) != 4 {
		t.Fatalf("after discard: %+v", v.Plan)
	}
}

func TestRisksMustBeAcknowledged(t *testing.T) {
	f := newFixture(t)
	v := f.ok("delete", `{"id":0}`).View
	if len(v.Plan.Risks) != 1 || v.Plan.Risks[0].Code != "booted-entry-lost" {
		t.Fatalf("risks = %+v", v.Plan.Risks)
	}
	f.fails("apply", `{}`, "confirmed")
	if _, err := f.store.Get("Boot0000"); err != nil {
		t.Fatal("refused apply deleted the entry")
	}
	f.ok("apply", `{"acknowledge":true}`)
	if _, err := f.store.Get("Boot0000"); err == nil {
		t.Fatal("acknowledged apply did not delete the entry")
	}
}

func TestCreateValidation(t *testing.T) {
	f := newFixture(t)
	f.fails("create", `{"description":"x","path":"\\EFI\\nope\\missing.efi","partition":"volume:sim-esp"}`, "does not exist")
	f.fails("create", `{"description":"x","path":"\\EFI\\Boot\\bootx64.efi"}`, "partition")
	f.fails("create", `{"description":"x","path":"\\EFI\\Boot\\bootx64.efi","partition":"volume:gone"}`, "no longer available")
	f.fails("create", `{"description":"","path":"\\EFI\\Boot\\bootx64.efi","partition":"volume:sim-esp"}`, "name must not be empty")
	f.fails("create", `{"description":"A\u0000B","path":"\\EFI\\Boot\\bootx64.efi","partition":"volume:sim-esp"}`, "control character")
	f.fails("create", `{"description":"x","path":"","partition":"volume:sim-esp"}`, "path")
	f.fails("create", `{"description":"x","path":"\\EFI\\Boot\\bootx64.efi","partition":"entry:Boot0003"}`, "GPT partition")
	f.fails("nope", `{}`, "unknown method")
	f.fails("create", `not json`, "invalid request")
	if v := f.ok("state", "").View; len(v.Plan.Writes) != 0 {
		t.Fatalf("rejected requests staged something: %+v", v.Plan.Writes)
	}

	// Reusing the partition of an entry checks the file when the partition
	// is one of the readable volumes.
	f.fails("create", `{"description":"x","path":"\\EFI\\missing.efi","partition":"entry:Boot0000"}`, "does not exist")
	res := f.ok("create", `{"description":"Second Windows","path":"\\EFI\\Microsoft\\Boot\\bootmgfw.efi","partition":"entry:Boot0000"}`)
	if e := find(res.View.Draft, res.ID); e.PartitionGUID != esp.SimulatedHD.Signature.String() || !e.Active {
		t.Fatalf("entry = %+v", e)
	}
}

func TestEdit(t *testing.T) {
	f := newFixture(t)
	before, _ := f.store.Get("Boot0000")

	v := f.ok("edit", `{"id":0,"description":"Windows 11","partition":"keep","path":"\\EFI\\Microsoft\\Boot\\bootmgfw.efi"}`).View
	e := find(v.Draft, 0)
	if e.Description != "Windows 11" || e.OptionalDataLen != 12 {
		t.Fatalf("rename lost data: %+v", e)
	}
	if len(v.Plan.Risks) != 0 {
		t.Fatalf("renaming the booted entry is not a risk: %+v", v.Plan.Risks)
	}

	v = f.ok("edit", `{"id":0,"description":"Windows 11","partition":"keep","path":"\\EFI\\Microsoft\\Boot\\bootmgr.efi"}`).View
	if e := find(v.Draft, 0); e.FilePath != `\EFI\Microsoft\Boot\bootmgr.efi` || e.OptionalDataLen != 12 {
		t.Fatalf("entry = %+v", e)
	}
	if len(v.Plan.Risks) != 1 {
		t.Fatalf("repointing the booted entry must be flagged: %+v", v.Plan.Risks)
	}
	f.fails("edit", `{"id":0,"description":"Windows 11","partition":"keep","path":"\\EFI\\Microsoft\\Boot\\nope.efi"}`, "does not exist")

	v = f.ok("edit", `{"id":1,"description":"Installer","path":"\\EFI\\BOOT\\BOOTX64.EFI","partition":"volume:sim-usb"}`).View
	if e := find(v.Draft, 1); e.PartitionGUID != "{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}" || e.Description != "Installer" {
		t.Fatalf("entry = %+v", e)
	}

	v = f.ok("edit", `{"id":3,"description":"Network boot","path":""}`).View
	if e := find(v.Draft, 3); e.Description != "Network boot" || e.DevicePath != "MAC(...)/IPv4(...)" {
		t.Fatalf("entry = %+v", e)
	}
	f.fails("edit", `{"id":9,"description":"x"}`, "does not exist")

	after, _ := f.store.Get("Boot0000")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("edit wrote to the store before apply")
	}
}

// setEntry writes an entry the way firmware or another tool might have, past
// the checks uefimgr applies to the entries it creates.
func (f *fixture) setEntry(id uint16, desc string, dp efi.DevicePath, optional []byte) {
	f.t.Helper()
	opt := &efi.LoadOption{Description: desc, DevicePath: dp, OptionalData: optional}
	opt.SetActive(true)
	raw, err := opt.MarshalBinary()
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.Set(manager.VarName(id), raw); err != nil {
		f.t.Fatal(err)
	}
}

func rawPath(nodes ...[]byte) efi.DevicePath {
	var dp efi.DevicePath
	for _, n := range nodes {
		dp = append(dp, n...)
	}
	return append(dp, efi.EndNode()...)
}

// The edit dialog sends back what it was given when the user changes only
// part of it. None of these must stage more than the user changed.
func TestEditFirmwareShapedEntries(t *testing.T) {
	hd := esp.SimulatedHD.Bytes()

	t.Run("name with trailing space, submitted unchanged", func(t *testing.T) {
		f := newFixture(t)
		f.setEntry(1, "ubuntu ", rawPath(hd, efi.FilePathNode(`\EFI\ubuntu\shimx64.efi`)), nil)
		v := f.ok("edit", `{"id":1,"description":"ubuntu ","partition":"keep","path":"\\EFI\\ubuntu\\shimx64.efi"}`).View
		if len(v.Plan.Writes) != 0 {
			t.Fatalf("unchanged edit staged %+v", v.Plan.Writes)
		}
	})

	t.Run("booted entry path without leading backslash, renamed", func(t *testing.T) {
		f := newFixture(t)
		f.setEntry(0, "Windows Boot Manager", rawPath(hd, efi.FilePathNode(`EFI\Microsoft\Boot\bootmgfw.efi`)), []byte("WINDOWS"))
		v := f.ok("edit", `{"id":0,"description":"Windows 11","partition":"keep","path":"EFI\\Microsoft\\Boot\\bootmgfw.efi"}`).View
		if len(v.Plan.Writes) != 1 || !strings.HasSuffix(v.Plan.Writes[0].Detail, "(renamed)") {
			t.Fatalf("writes = %+v", v.Plan.Writes)
		}
		if len(v.Plan.Risks) != 0 {
			t.Fatalf("a rename must not look like losing the booted entry: %+v", v.Plan.Risks)
		}
	})

	t.Run("unnamed entry, repointed", func(t *testing.T) {
		f := newFixture(t)
		f.setEntry(2, "", rawPath(hd, efi.FilePathNode(`\EFI\Shell\Shell.efi`)), nil)
		v := f.ok("edit", `{"id":2,"description":"","partition":"keep","path":"\\EFI\\Boot\\bootx64.efi"}`).View
		if e := find(v.Draft, 2); e.Description != "" || e.FilePath != `\EFI\Boot\bootx64.efi` {
			t.Fatalf("entry = %+v", e)
		}
	})

	t.Run("node after the file path", func(t *testing.T) {
		f := newFixture(t)
		vendor := efi.Node{Type: 0x04, Subtype: 0x03, Data: make([]byte, 16)}.Bytes()
		f.setEntry(1, "ubuntu", rawPath(hd, efi.FilePathNode(`\EFI\ubuntu\shimx64.efi`), vendor), nil)
		if opts := f.ok("targets", `{"id":1}`); opts.Editable || !strings.Contains(opts.Reason, "only its name") {
			t.Fatalf("targets = %+v", opts)
		}
		f.fails("edit", `{"id":1,"description":"ubuntu","partition":"keep","path":"\\EFI\\ubuntu\\grubx64.efi"}`, "only its name")
		v := f.ok("edit", `{"id":1,"description":"Ubuntu","partition":"","path":""}`).View
		if e := find(v.Draft, 1); e.Description != "Ubuntu" || e.DevicePath != find(v.Current, 1).DevicePath {
			t.Fatalf("entry = %+v", e)
		}
	})
}

func TestTargetOptions(t *testing.T) {
	f := newFixture(t)
	keys := func(opts response) []string {
		var out []string
		for _, p := range opts.Partitions {
			out = append(out, p.Key)
		}
		return out
	}
	// Every demo entry is on the simulated ESP, so it is offered once, as a
	// volume that can be browsed.
	if got := keys(f.ok("targets", "")); !reflect.DeepEqual(got, []string{"volume:sim-esp", "volume:sim-usb"}) {
		t.Fatalf("new entry = %v", got)
	}
	opts := f.ok("targets", `{"id":0}`)
	if got := keys(opts); !reflect.DeepEqual(got, []string{"keep", "volume:sim-usb"}) {
		t.Fatalf("Boot0000 = %v", got)
	}
	if keep := opts.Partitions[0]; keep.Volume == nil || keep.Volume.ID != "sim-esp" {
		t.Fatalf("keep must be browsable on the ESP: %+v", keep)
	}
	f.fails("targets", `{"id":9}`, "does not exist")
}

func TestVolumesAndBrowse(t *testing.T) {
	f := newFixture(t)
	vols := f.ok("volumes", "").Volumes
	if len(vols) != 2 || !vols[0].IsESP || vols[0].PartitionGUID == "" {
		t.Fatalf("volumes = %+v", vols)
	}
	entries := f.ok("browse", `{"volume":"sim-esp","dir":"\\EFI\\ubuntu"}`).Entries
	if len(entries) != 2 || entries[0].Name != "grubx64.efi" {
		t.Fatalf("entries = %+v", entries)
	}
	f.fails("browse", `{"volume":"sim-esp","dir":"\\EFI\\..\\.."}`, "must not contain")
	f.fails("browse", `{"volume":"elsewhere","dir":"\\"}`, "unknown partition")
}

func TestBackupsRoundTrip(t *testing.T) {
	f := newFixture(t)
	saved := f.ok("backupNow", "")
	if len(saved.Backups) != 1 || saved.Backups[0].Kind != "manual" {
		t.Fatalf("backups = %+v", saved.Backups)
	}
	f.ok("delete", `{"id":1}`)
	f.ok("delete", `{"id":7}`)
	f.ok("apply", `{}`)

	v := f.ok("loadBackup", `{"name":"`+filepath.Base(saved.Path)+`"}`).View
	if len(v.Draft.Entries) != 4 || len(v.Draft.Unordered) != 1 {
		t.Fatalf("staged restore = %+v / %+v", v.Draft.Order, v.Draft.Unordered)
	}
	if len(v.Current.Entries) != 3 {
		t.Fatal("loading a backup must only stage it")
	}
	f.ok("apply", `{}`)

	raw, err := os.ReadFile(saved.Path)
	if err != nil {
		t.Fatal(err)
	}
	f.ok("delete", `{"id":2}`)
	body, _ := json.Marshal(map[string]string{"snapshot": string(raw)})
	v = f.ok("loadSnapshot", string(body)).View
	if len(v.Plan.Writes) != 0 {
		t.Fatalf("loading the matching backup replaces what was staged: %+v", v.Plan.Writes)
	}
	f.fails("loadSnapshot", `{"snapshot":"{}"}`, "format")
	f.fails("loadBackup", `{"name":"../vars.json"}`, "not a backup file name")
}

func TestOpenBackups(t *testing.T) {
	f := newFixture(t)
	f.fails("openBackups", "", "cannot be opened")
	var opened string
	f.api.OpenFolder = func(p string) error { opened = p; return nil }
	f.ok("openBackups", "")
	if opened != filepath.Join(f.dir, "backups") {
		t.Fatalf("opened %q", opened)
	}
}

func httpCall(t *testing.T, ts *httptest.Server, method, path, token, host, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("X-Uefimgr-Token", token)
	}
	if host != "" {
		req.Host = host
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestHTTPTransport(t *testing.T) {
	f := newFixture(t)
	srv, err := ui.NewServer(f.api)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	token := srv.Token()

	if code, body := httpCall(t, ts, "GET", "/", "", "", ""); code != 200 || !strings.Contains(body, "Boot sequence") {
		t.Fatalf("index: %d", code)
	}
	if code, _ := httpCall(t, ts, "GET", "/other", "", "", ""); code != 404 {
		t.Fatalf("unknown path: %d", code)
	}
	if code, _ := httpCall(t, ts, "POST", "/api/state", "", "", ""); code != 403 {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := httpCall(t, ts, "POST", "/api/state?token="+token, "", "", ""); code != 403 {
		t.Fatalf("a token in the address must not be accepted: %d", code)
	}
	if code, _ := httpCall(t, ts, "POST", "/api/state", token, "evil.example", ""); code != 403 {
		t.Fatalf("foreign Host: %d", code)
	}
	if code, _ := httpCall(t, ts, "GET", "/api/state", token, "", ""); code != 405 {
		t.Fatalf("GET: %d", code)
	}
	code, body := httpCall(t, ts, "POST", "/api/state", token, "", "")
	if code != 200 || !strings.Contains(body, `"Windows Boot Manager"`) {
		t.Fatalf("state: %d %s", code, body)
	}
	code, body = httpCall(t, ts, "POST", "/api/delete", token, "", `{"id":99}`)
	if code != 400 || !strings.Contains(body, "does not exist") {
		t.Fatalf("error: %d %s", code, body)
	}
}

func TestPageIsSelfContained(t *testing.T) {
	for _, needle := range []string{"http://", "https://", "//cdn", "@import"} {
		if strings.Contains(ui.IndexHTML, needle) {
			t.Errorf("the page must not load anything from the network, found %q", needle)
		}
	}
	if !strings.Contains(ui.IndexHTML, "<head>") {
		t.Error("the window host injects its bridge after <head>")
	}
}

func TestDemoSeedIsValid(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"Boot0000", "Boot0001", "Boot0002", "Boot0003", "Boot0007"} {
		raw, err := f.store.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := efi.ParseLoadOption(raw); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
