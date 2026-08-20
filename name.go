package download

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// https://regex101.com/r/N4AovD/3
var reContentDisposition = regexp.MustCompile(`filename[^;\n=]*=(['"](.*?)['"]|[^;\n]*)`)

// parseContentDisposition extracts a filename from a Content-Disposition
// header value, handling quoted values and RFC 5987 extended encoding. Only
// the RFC 5987 form is percent-encoded; plain and quoted filenames are
// returned verbatim.
func parseContentDisposition(input string) string {
	group := reContentDisposition.FindStringSubmatch(input)
	if len(group) != 3 {
		return ""
	}
	if group[2] != "" {
		return group[2]
	}
	b, a, found := strings.Cut(group[1], "''")
	if found && strings.EqualFold(b, "utf-8") {
		decoded, err := url.PathUnescape(a)
		if err != nil {
			return ""
		}
		return decoded
	}
	if b != `""` {
		return b
	}
	return ""
}

// deriveName resolves the output filename from the final response location
// and headers: Content-Disposition wins, then the URL path base. u.Path is
// already percent-decoded by url.Parse, so no further decoding happens (a
// second pass would corrupt names containing '+' or literal '%').
func deriveName(location string, header http.Header) (string, error) {
	if name := parseContentDisposition(header.Get("Content-Disposition")); name != "" {
		return filepath.Base(name), nil
	}
	u, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("parse location %q: %w", location, err)
	}
	name := u.Path
	if name == "" {
		name = u.Opaque
	}
	if base := filepath.Base(name); base != "." && base != string(filepath.Separator) && base != "/" {
		return base, nil
	}
	return "", nil
}

// resolveDest turns the user-supplied dest into a concrete file path.
// dest may be empty (derived name in the current directory), an existing
// directory (derived name inside it), or a file path used as-is.
func resolveDest(dest, location string, header http.Header) (string, error) {
	dir := ""
	if dest != "" {
		fi, err := os.Stat(dest)
		if err != nil || !fi.IsDir() {
			return dest, nil
		}
		dir = dest
	}
	name, err := deriveName(location, header)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", fmt.Errorf("cannot derive filename from %q: pass an explicit file path", location)
	}
	return filepath.Join(dir, name), nil
}
