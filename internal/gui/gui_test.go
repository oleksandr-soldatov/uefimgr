package gui

import (
	"errors"
	"strings"
	"testing"
)

func TestWithBridgeRunsBeforeThePage(t *testing.T) {
	html := "<!doctype html><html><head><title>x</title></head><body><script>page()</script></body></html>"
	got := withBridge(html)
	bridge := strings.Index(got, "window.__uefimgr")
	page := strings.Index(got, "page()")
	if bridge < 0 || page < 0 || bridge > page {
		t.Fatalf("bridge must come before the page's script:\n%s", got)
	}
	if strings.Count(got, "<head>") != 1 {
		t.Fatal("head duplicated")
	}
}

func TestSettleScript(t *testing.T) {
	ok := settleScript(7, map[string]any{"name": "Boot0001", "text": "a</script> b"}, nil)
	if !strings.HasPrefix(ok, "window.__uefimgr._settle(7,true,{") {
		t.Fatalf("script = %s", ok)
	}
	// The answer is evaluated as JavaScript, so nothing in it may end the
	// script element or a string literal early.
	if strings.Contains(ok, "</script>") || strings.Contains(ok, " ") {
		t.Fatalf("unsafe characters survived: %s", ok)
	}

	failed := settleScript(8, nil, errors.New(`no "such" entry`))
	if failed != `window.__uefimgr._settle(8,false,"no \"such\" entry")` {
		t.Fatalf("script = %s", failed)
	}

	bad := settleScript(9, make(chan int), nil)
	if !strings.HasPrefix(bad, "window.__uefimgr._settle(9,false,") {
		t.Fatalf("an unencodable answer must reject: %s", bad)
	}
}
