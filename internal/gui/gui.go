// Package gui is the native side of the interface: the application window
// that hosts the page, message boxes, and opening things in the shell.
package gui

import (
	"encoding/json"
	"strings"
)

// Handler answers a method call made by the page.
type Handler func(method string, params json.RawMessage) (any, error)

// WindowOptions describes the application window.
type WindowOptions struct {
	Title string
	// HTML is the page. It reaches the Handler through window.__uefimgr.
	HTML    string
	Handler Handler
	// DataDir is where the embedded browser keeps its profile.
	DataDir string
	Width   int
	Height  int
	// Debug enables the browser's developer tools and context menu.
	Debug bool
}

// bridgeJS gives the page window.__uefimgr.call(method, params), which
// returns a promise. Requests go out as JSON strings; the host answers by
// calling _settle.
const bridgeJS = `(function () {
  var pending = {}, seq = 0;
  window.__uefimgr = {
    call: function (method, params) {
      return new Promise(function (resolve, reject) {
        var id = ++seq;
        pending[id] = { resolve: resolve, reject: reject };
        window.chrome.webview.postMessage(JSON.stringify({ id: id, method: method, params: params || {} }));
      });
    },
    _settle: function (id, ok, value) {
      var p = pending[id];
      if (!p) return;
      delete pending[id];
      if (ok) p.resolve(value); else p.reject(new Error(value));
    }
  };
})();`

type bridgeRequest struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// withBridge puts the bridge script first in the page, so it exists before
// any of the page's own code runs.
func withBridge(html string) string {
	return strings.Replace(html, "<head>", "<head>\n<script>"+bridgeJS+"</script>", 1)
}

// settleScript builds the JavaScript that delivers one answer to the page.
func settleScript(id int, value any, callErr error) string {
	ok := callErr == nil
	if !ok {
		value = callErr.Error()
	}
	payload, err := json.Marshal(value)
	if err != nil {
		ok = false
		payload, _ = json.Marshal("uefimgr could not encode its answer: " + err.Error())
	}
	head, _ := json.Marshal([]any{id, ok})
	// head is "[id,ok]"; splice the payload in as the third argument.
	return "window.__uefimgr._settle(" + string(head[1:len(head)-1]) + "," + string(payload) + ")"
}
