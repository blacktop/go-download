//go:build !windows

package download

import "github.com/vbauerster/mpb/v8"

func barRefiller(style mpb.BarStyleComposer) mpb.BarStyleComposer {
	green := func(s string) string {
		return "\x1b[32m" + s + "\x1b[0m"
	}
	return style.RefillerMeta(green)
}

func barStyle() mpb.BarStyleComposer {
	return mpb.BarStyle().Lbound("[").Rbound("|").Refiller("#").Filler("=").Tip(">").Padding("-")
}
