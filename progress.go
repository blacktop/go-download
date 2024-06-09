package download

import (
	"fmt"
	"io"
	"math"
	"sync/atomic"
	"time"

	"github.com/VividCortex/ewma"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

var (
	_ decor.Decorator     = (*mainDecorator)(nil)
	_ decor.Decorator     = (*flashDecorator)(nil)
	_ decor.Wrapper       = (*flashDecorator)(nil)
	_ decor.Decorator     = (*peak)(nil)
	_ decor.EwmaDecorator = (*peak)(nil)
)

type message struct {
	msg   string
	isErr bool
}

func newFlashDecorator(decorator decor.Decorator, msgCh <-chan message, limit uint) decor.Decorator {
	if decorator == nil {
		return nil
	}
	d := &flashDecorator{
		Decorator: decorator,
		msgCh:     msgCh,
		limit:     limit,
	}
	return d
}

type flashDecorator struct {
	decor.Decorator
	msgCh <-chan message
	limit uint
	count uint
	msg   message
}

func (d *flashDecorator) Unwrap() decor.Decorator {
	return d.Decorator
}

func (d *flashDecorator) Decor(stat decor.Statistics) (string, int) {
	if d.count == 0 {
		select {
		case msg := <-d.msgCh:
			d.count = d.limit
			d.msg = msg
		default:
			return d.Decorator.Decor(stat)
		}
	} else {
		d.count--
	}
	if d.msg.isErr {
		_, _ = d.Format("")
		return d.msg.msg, math.MaxInt
	}
	return d.Format(d.msg.msg)
}

type mainDecorator struct {
	decor.WC
	name   string
	format string
	curTry *uint32
}

func newMainDecorator(curTry *uint32, name, format string, wc decor.WC) decor.Decorator {
	d := &mainDecorator{
		WC:     wc.Init(),
		name:   name,
		format: format,
		curTry: curTry,
	}
	return d
}

func (d *mainDecorator) Decor(stat decor.Statistics) (string, int) {
	var name string
	if atomic.LoadUint32(&globTry) != 0 {
		name = fmt.Sprintf("%s:R%02d", d.name, atomic.LoadUint32(d.curTry))
	} else {
		name = d.name
	}
	return d.Format(fmt.Sprintf(d.format, name, decor.SizeB1024(stat.Total)))
}

type peak struct {
	decor.WC
	format   string
	msg      string
	min      float64
	updCount uint8
	zDur     time.Duration
	mean     ewma.MovingAverage
}

func newSpeedPeak(format string, wc decor.WC) decor.Decorator {
	d := &peak{
		WC:     wc.Init(),
		format: format,
		mean:   ewma.NewMovingAverage(30),
	}
	return d
}

func (s *peak) EwmaUpdate(n int64, dur time.Duration) {
	if n <= 0 {
		s.zDur += dur
		return
	}
	durPerByte := float64(s.zDur+dur) / float64(n)
	if math.IsInf(durPerByte, 0) || math.IsNaN(durPerByte) {
		s.zDur += dur
		return
	}
	s.zDur = 0
	s.mean.Add(durPerByte)
	switch s.updCount {
	case ewma.WARMUP_SAMPLES:
		durPerByte = s.mean.Value()
		if s.min == 0 || durPerByte < s.min {
			s.min = durPerByte
		}
	default:
		s.updCount++
	}
}

func (s *peak) Decor(stat decor.Statistics) (string, int) {
	if stat.Completed && s.msg == "" {
		if s.min == 0 {
			s.msg = "N/A"
		} else {
			s.msg = fmt.Sprintf(s.format, decor.FmtAsSpeed(decor.SizeB1024(math.Round(1e9/s.min))))
		}
	}
	return s.Format(s.msg)
}

type progressBar struct {
	*mpb.Bar
	msgHandler  func(message)
	prefix      string
	initialized atomic.Bool
}

func (b *progressBar) flash(msg string) {
	msg = fmt.Sprintf("%s %s", b.prefix, msg)
	b.msgHandler(message{
		msg:   msg,
		isErr: false,
	})
}

func (b *progressBar) flashErr(msg string) {
	b.msgHandler(message{
		msg:   fmt.Sprintf("%s:ERR %s", b.prefix, msg),
		isErr: true,
	})
}

func (b *progressBar) init(p *Part, curTry *uint32) error {
	if b.initialized.Load() {
		return nil
	}
	var barBuilder mpb.BarFillerBuilder
	msgCh := make(chan message, 1)
	total := p.total()
	if total < 0 {
		total = 0
		barBuilder = mpb.NopStyle()
	} else {
		barBuilder = barRefiller(barStyle())
	}
	p.logger.Debug("Setting Bar Total", "total", total)
	bar, err := p.progress.Add(total, barBuilder.Build(), mpb.BarFillerTrim(), mpb.BarPriority(p.order),
		mpb.BarOptional(
			mpb.BarExtender(mpb.BarFillerFunc(
				func(w io.Writer, _ decor.Statistics) error {
					_, err := fmt.Fprintln(w)
					return err
				}), true),
			p.single),
		mpb.PrependDecorators(
			newFlashDecorator(newMainDecorator(curTry, p.name, "%s %.1f", decor.WCSyncWidthR), msgCh, 15),
			decor.Conditional(total == 0,
				decor.OnComplete(decor.Spinner([]string{`-`, `\`, `|`, `/`}, decor.WC{C: decor.DextraSpace}), "✅ "),
				decor.OnComplete(decor.NewPercentage("%.2f", decor.WCSyncSpace), "✅")),
		),
		mpb.AppendDecorators(
			decor.Conditional(total == 0,
				decor.Name(""),
				decor.OnCompleteOrOnAbort(decor.NewAverageETA(
					decor.ET_STYLE_MMSS,
					time.Now(),
					decor.FixedIntervalTimeNormalizer(30),
					decor.WCSyncWidth,
				), ":"),
			),
			decor.EwmaSpeed(decor.SizeB1024(0), "%.1f", 30, decor.WCSyncSpace),
			decor.OnComplete(decor.Name("", decor.WCSyncSpace), "Peak:"),
			newSpeedPeak("%.1f", decor.WCSyncSpace),
		),
	)
	if err != nil {
		return err
	}
	if p.Written != 0 {
		p.logger.Debug("Setting Bar Current", "written", p.Written)
		bar.SetCurrent(p.Written)
		bar.DecoratorAverageAdjust(time.Now().Add(-p.Elapsed))
	}
	b.Bar = bar
	b.prefix = p.name
	b.msgHandler = p.makeMsgHandler(msgCh)
	b.initialized.Store(true)
	return nil
}
