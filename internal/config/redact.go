package config

import "net/url"

// UnparseableURL is what RedactURL renders for an input it cannot safely
// redact.
const UnparseableURL = "<unparseable url>"

// RedactURL renders an upstream URL for a log line or an error message with
// any userinfo password replaced (url.URL.Redacted keeps the user name and
// prints the password as "xxxxx").
//
// Only a host-bearing URL is echoed. An input url.Parse rejects is not, nor is
// an opaque one ("user:pass@host:8428" parses as scheme "user" with opaque
// "pass@host:8428") or a hostless one ("http:/user:pass@host" keeps the
// credentials in the path): Redacted only rewrites a parsed userinfo, so it
// would print either whole.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" {
		return UnparseableURL
	}
	return u.Redacted()
}
