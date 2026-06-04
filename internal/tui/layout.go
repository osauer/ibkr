package tui

type rect struct {
	x int
	y int
	w int
	h int
}

type screenLayout struct {
	status      rect
	ticker      rect
	output      rect
	warning     rect
	prompt      rect
	showTicker  bool
	showWarning bool
}

func computeLayout(size Size) screenLayout {
	if size.Rows < 8 {
		size.Rows = 8
	}
	if size.Cols < 40 {
		size.Cols = 40
	}
	l := screenLayout{
		status: rect{x: 0, y: 0, w: size.Cols, h: 1},
		prompt: rect{x: 0, y: size.Rows - 2, w: size.Cols, h: 2},
	}
	bodyTop := 1
	if size.Rows >= 12 {
		l.showTicker = true
		l.ticker = rect{x: 0, y: 1, w: size.Cols, h: 1}
		bodyTop = 2
	}
	bodyBottom := l.prompt.y
	if size.Cols >= 100 && size.Rows >= 18 {
		l.showWarning = true
		warnW := min(36, max(28, size.Cols/3))
		l.warning = rect{x: size.Cols - warnW, y: bodyTop, w: warnW, h: min(7, bodyBottom-bodyTop)}
		l.output = rect{x: 0, y: bodyTop, w: size.Cols - warnW - 1, h: bodyBottom - bodyTop}
	} else {
		l.output = rect{x: 0, y: bodyTop, w: size.Cols, h: bodyBottom - bodyTop}
	}
	if l.output.h < 1 {
		l.output.h = 1
	}
	return l
}
