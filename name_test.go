package download

import (
	"net/http"
	"testing"
)

func TestParseContentDisposition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input  string
		output string
	}{
		{input: "", output: ""},
		{input: "garbage", output: ""},
		{input: "attachment; filename=", output: ""},
		{input: "attachment; filename=''", output: ""},
		{input: `attachment; filename=""`, output: ""},
		{input: "attachment; garbage=filename", output: ""},
		{input: "attachment; filename=filename", output: "filename"},
		{input: "attachment; filename=content.txt", output: "content.txt"},
		{input: "attachment; filename='content.txt'", output: "content.txt"},
		{input: `attachment; filename="content.txt"`, output: "content.txt"},
		{input: "attachment; filename*=UTF-8''content.txt", output: "content.txt"},
	}

	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			t.Parallel()
			if output := parseContentDisposition(test.input); output != test.output {
				t.Errorf("expected %q got %q", test.output, output)
			}
		})
	}
}

func TestDeriveName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		location string
		header   string
		expected string
	}{
		{name: "empty", location: "", expected: ""},
		{name: "host only", location: "http://example.org", expected: ""},
		{name: "trailing slash", location: "http://example.org/", expected: ""},
		{name: "path", location: "http://example.org/abc", expected: "abc"},
		{name: "escaped path", location: "http://example.org/abc%20d", expected: "abc d"},
		{name: "nested path", location: "http://example.org/a/b/c.ipsw", expected: "c.ipsw"},
		{name: "plus is not a space", location: "http://example.org/a+b.zip", expected: "a+b.zip"},
		{
			name:     "no double decode",
			location: "http://example.org/file%2520name.zip",
			expected: "file%20name.zip",
		},
		{name: "literal percent", location: "http://example.org/100%25off.zip", expected: "100%off.zip"},
		{
			name:     "content disposition wins",
			location: "http://example.org/abc",
			header:   "attachment; filename*=utf-8''%e2%82%ac%20rates",
			expected: "€ rates",
		},
		{
			name:     "content disposition plain",
			location: "http://example.org/abc",
			header:   `attachment; filename="report.pdf"`,
			expected: "report.pdf",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := make(http.Header)
			if test.header != "" {
				h.Add("Content-Disposition", test.header)
			}
			output, err := deriveName(test.location, h)
			if err != nil {
				t.Fatal(err)
			}
			if output != test.expected {
				t.Errorf("expected %q got %q", test.expected, output)
			}
		})
	}
}

func TestResolveDest(t *testing.T) {
	t.Parallel()
	h := make(http.Header)

	t.Run("explicit file path", func(t *testing.T) {
		t.Parallel()
		got, err := resolveDest("out/file.bin", "http://example.org/abc", h)
		if err != nil {
			t.Fatal(err)
		}
		if got != "out/file.bin" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("directory dest", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		got, err := resolveDest(dir, "http://example.org/abc", h)
		if err != nil {
			t.Fatal(err)
		}
		if want := dir + "/abc"; got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})

	t.Run("empty dest derives name", func(t *testing.T) {
		t.Parallel()
		got, err := resolveDest("", "http://example.org/file.zip", h)
		if err != nil {
			t.Fatal(err)
		}
		if got != "file.zip" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("underivable errors", func(t *testing.T) {
		t.Parallel()
		if _, err := resolveDest("", "http://example.org/", h); err == nil {
			t.Error("expected error")
		}
	})
}
