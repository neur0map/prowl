// Package callback renders the browser page a user lands on at the end of
// an OAuth redirect flow.
//
// The page is the only part of authorization the user sees outside the
// terminal, so it is worth more than a line of plain text: it reports
// whether authorization worked, names what was authorized, explains any
// failure in the provider's own words, and offers to close itself.
//
// Rendering is self-contained. Markup, styles, script, and artwork are
// embedded in the binary. The page makes no external asset requests.
package callback

import (
	_ "embed"
	"encoding/base64"
	"html/template"
	"io"
	"net/http"
	"time"
)

//go:embed page.html
var markup string

//go:embed page.css
var stylesheet string

//go:embed page.js
var script string

//go:embed prowl.svg
var wordmark string

//go:embed prowl-mark.svg
var mark string

//go:embed manrope-latin.woff2
var font string

//go:embed Manrope-OFL.txt
var fontLicense string

// closeDelay is how long the page counts down before asking the browser to
// close the tab. Long enough to read the outcome, short enough not to feel
// like waiting.
const closeDelay = 5 * time.Second

// tmpl is parsed once at startup. A parse failure means the embedded
// template is broken, which is a build-time mistake rather than anything a
// user can cause, so panicking here fails fast and loudly.
var (
	tmpl            = template.Must(template.New("page.html").Parse(markup))
	fontURI         = template.URL("data:font/woff2;base64," + base64.StdEncoding.EncodeToString([]byte(font)))
	favicon         = template.URL("data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(mark)))
	fontLicenseHTML = template.HTML("<!--\n" + fontLicense + "\n-->")
)

// Result describes the outcome of an authorization attempt.
type Result struct {
	// Subject names what was being authorized, such as an MCP server name.
	// Optional; when empty the page simply omits it.
	Subject string

	// ErrorCode is the OAuth error code (for example "access_denied").
	// A non-empty value renders the page in its failure state.
	ErrorCode string

	// ErrorDescription is the provider's human-readable explanation. It is
	// shown alongside ErrorCode and may be empty.
	ErrorDescription string
}

// Failed reports whether the result describes a failed authorization.
func (r Result) Failed() bool { return r.ErrorCode != "" }

// Write renders the callback page for the given result to w. It always
// writes a complete page: if template execution somehow fails midway, the
// error is returned so the caller can log it, but the user is never shown
// a blank tab.
func Write(w io.Writer, r Result) error {
	data := struct {
		Title            string
		Kind             string
		Heading          string
		Detail           string
		Subject          string
		ErrorCode        string
		ErrorDescription string
		Status           string
		CloseDelay       int
		CSS              template.CSS
		JS               template.JS
		Mark             template.HTML
		Wordmark         template.HTML
		Font             template.URL
		FontLicense      template.HTML
		Favicon          template.URL
	}{
		Subject:          r.Subject,
		ErrorCode:        r.ErrorCode,
		ErrorDescription: r.ErrorDescription,
		CSS:              template.CSS(stylesheet),
		JS:               template.JS(script),
		Mark:             template.HTML(mark),
		Wordmark:         template.HTML(wordmark),
		Font:             fontURI,
		FontLicense:      fontLicenseHTML,
		Favicon:          favicon,
	}

	if r.Failed() {
		data.Title = "Authorization failed - Prowl"
		data.Kind = "failed"
		data.Heading = "Not connected."
		data.Detail = "Prowl was not granted access to"
		if r.Subject == "" {
			data.Detail = "Prowl was not granted access."
		}
		// A failed page keeps itself open so the reader can read the reason.
		data.Status = "Close this tab when you’re ready."
	} else {
		data.Title = "Authorized - Prowl"
		data.Kind = "ok"
		data.Heading = "You’re connected."
		data.Detail = "Prowl is now connected to"
		if r.Subject == "" {
			data.Detail = "Prowl is now connected."
		}
		// This remains readable without JavaScript or if closing is blocked.
		data.Status = "You can close this tab."
		data.CloseDelay = int(closeDelay.Seconds())
	}

	return tmpl.Execute(w, data)
}

// Serve writes the callback page as a complete HTTP response, choosing a
// status code that matches the outcome. Errors are logged by the caller;
// the page itself is best effort because by this point the browser is
// already committed to rendering whatever arrives.
func Serve(w http.ResponseWriter, r Result) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page reflects a one-time authorization result and must never be
	// replayed from cache on a later visit to the same localhost URL.
	w.Header().Set("Cache-Control", "no-store")
	if r.Failed() {
		w.WriteHeader(http.StatusBadRequest)
	}
	return Write(w, r)
}
