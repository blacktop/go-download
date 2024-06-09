/*
Copyright © 2024 blacktop

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
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/blacktop/go-download"
	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.Flags().BoolP("verbose", "V", false, "verbose output")
	rootCmd.Flags().BoolP("progess", "b", false, "progress bar")
	rootCmd.Flags().IntP("parts", "p", 1, "number of parts")
}

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:           "dl",
	Short:         "Download a file from the internet in parts",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		verbose, _ := cmd.Flags().GetBool("verbose")
		progress, _ := cmd.Flags().GetBool("progress")
		parts, _ := cmd.Flags().GetInt("parts")

		logger := log.NewWithOptions(os.Stdout, log.Options{
			ReportCaller:    true,
			ReportTimestamp: true,
			TimeFormat:      time.Kitchen,
			Prefix:          "Downloading ⬇️",
		})
		if verbose {
			logger.SetLevel(log.DebugLevel)
		}

		mgr, err := download.New(&download.Config{
			Context:  ctx,
			Logger:   slog.New(logger),
			Progress: progress,
			Verbose:  verbose,
			Parts:    parts,
		})
		if err != nil {
			return err
		}

		return mgr.Get(args[0])
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
}

// func init() {
// 	log.SetHandler(log.Default)
// }
