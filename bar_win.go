//go:build windows

package download

import "github.com/vbauerster/mpb/v8"

func barRefiller(style mpb.BarStyleComposer) mpb.BarStyleComposer {
	return style.Refiller("$")
}

func barStyle() mpb.BarStyleComposer {
	return mpb.BarStyle().Lbound(" ").Rbound(" ").Refiller("#").Filler("#").Tip("")
}
