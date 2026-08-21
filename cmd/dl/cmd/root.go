/*
Copyright © 2026 blacktop

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/
package cmd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/blacktop/go-download"
	charmlog "github.com/charmbracelet/log"
	"github.com/spf13/cobra"
)

var log = charmlog.NewWithOptions(os.Stderr, charmlog.Options{
	ReportTimestamp: true,
	TimeFormat:      time.Kitchen,
})

// rampDecisionLogHandler keeps the evidence file limited to the stable ramp
// schema. Other debug records may contain local paths or endpoint metadata.
type rampDecisionLogHandler struct{ slog.Handler }

func (h rampDecisionLogHandler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Message != "ramp decision" {
		return nil
	}
	return h.Handler.Handle(ctx, rec)
}

func (h rampDecisionLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return rampDecisionLogHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h rampDecisionLogHandler) WithGroup(name string) slog.Handler {
	return rampDecisionLogHandler{Handler: h.Handler.WithGroup(name)}
}

// fanoutLogHandler delivers each record to every handler that wants it, so
// the JSON evidence sink never replaces the human --verbose diagnostics.
type fanoutLogHandler struct{ handlers []slog.Handler }

func (f fanoutLogHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	for _, h := range f.handlers {
		if h.Enabled(ctx, lvl) {
			return true
		}
	}
	return false
}

func (f fanoutLogHandler) Handle(ctx context.Context, rec slog.Record) error {
	var firstErr error
	for _, h := range f.handlers {
		if !h.Enabled(ctx, rec.Level) {
			continue
		}
		if err := h.Handle(ctx, rec.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (f fanoutLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		next[i] = h.WithAttrs(attrs)
	}
	return fanoutLogHandler{handlers: next}
}

func (f fanoutLogHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		next[i] = h.WithGroup(name)
	}
	return fanoutLogHandler{handlers: next}
}

var flags struct {
	output   string
	parts    int
	timeout  time.Duration
	retries  int
	headers  []string
	sha256   string
	force    bool
	quiet    bool
	insecure bool
	verbose  bool
}

func init() {
	rootCmd.Flags().StringVarP(&flags.output, "output", "o", "",
		"output file or directory (default: derived from URL)")
	rootCmd.Flags().IntVarP(&flags.parts, "parts", "p", 8, "number of parallel connections")
	rootCmd.Flags().DurationVar(&flags.timeout, "timeout", 0, "per-read stall timeout (default 15s)")
	rootCmd.Flags().IntVar(&flags.retries, "retries", 0, "per-chunk retry budget (default 10)")
	rootCmd.Flags().StringArrayVarP(&flags.headers, "header", "H", nil,
		"extra header, 'Key: Value' (repeatable)")
	rootCmd.Flags().StringVar(&flags.sha256, "sha256", "",
		"expected sha256 (hex); verified before rename")
	rootCmd.Flags().BoolVarP(&flags.force, "force", "f", false, "overwrite existing destination")
	rootCmd.Flags().BoolVarP(&flags.quiet, "quiet", "q", false, "no progress output")
	rootCmd.Flags().BoolVar(&flags.insecure, "insecure", false, "skip TLS certificate verification")
	rootCmd.Flags().BoolVarP(&flags.verbose, "verbose", "V", false, "verbose output")
}

var rootCmd = &cobra.Command{
	Use:           "dl <url>",
	Short:         "Download a file as fast as possible: parallel parts, CDN node racing, resume",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if flags.verbose {
			log.SetLevel(charmlog.DebugLevel)
		}

		headers, err := parseHeaders(flags.headers)
		if err != nil {
			return err
		}

		logger := slog.New(log)
		// DL_LOG_JSON routes ramp decision records to a private JSONL evidence
		// file; the human CLI rendering is never a parser contract. The sink
		// fans out beside the human logger so --verbose diagnostics still
		// reach stderr when both are requested.
		if path := os.Getenv("DL_LOG_JSON"); path != "" {
			f, ferr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if ferr != nil {
				return fmt.Errorf("DL_LOG_JSON: %w", ferr)
			}
			defer f.Close()
			logger = slog.New(fanoutLogHandler{handlers: []slog.Handler{
				log,
				rampDecisionLogHandler{Handler: slog.NewJSONHandler(f,
					&slog.HandlerOptions{Level: slog.LevelDebug})},
			}})
		}

		opt := &download.Options{
			Parts:          flags.parts,
			Timeout:        flags.timeout,
			MaxRetries:     flags.retries,
			Headers:        headers,
			ExpectedSHA256: flags.sha256,
			Overwrite:      flags.force,
			Logger:         logger,
		}
		if flags.insecure {
			opt.TLSConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- user opted in
		}
		if !flags.quiet {
			opt.Reporter = newMpbReporter()
		}

		dl, err := download.New(opt)
		if err != nil {
			return err
		}

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		res, err := dl.Get(ctx, args[0], flags.output)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				log.Warn("interrupted — rerun the same command to resume")
			}
			return err
		}

		speed := float64(res.Size) / max(res.Elapsed.Seconds(), 0.001) / (1 << 20)
		summary := []any{
			"path", res.Path,
			"size", fmt.Sprintf("%.1f MiB", float64(res.Size)/(1<<20)),
			"elapsed", res.Elapsed.Round(time.Millisecond),
			"speed", fmt.Sprintf("%.1f MiB/s", speed),
		}
		if res.Resumed {
			summary = append(summary, "resumed", true)
		}
		if res.SHA256 != "" {
			summary = append(summary, "sha256", "verified")
		}
		log.Info("downloaded", summary...)
		return nil
	},
}

func parseHeaders(raw []string) (http.Header, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	h := make(http.Header, len(raw))
	for _, kv := range raw {
		k, v, ok := strings.Cut(kv, ":")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("invalid header %q: want 'Key: Value'", kv)
		}
		h.Add(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	return h, nil
}

// Execute runs the root command. Called once from main.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
}
