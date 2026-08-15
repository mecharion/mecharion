package authn

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
)

// 滑块的尺寸。
const (
	bgWidth   = 320
	bgHeight  = 160
	pieceSize = 52
	// sliderMinX 让缺口不会紧贴左边——贴边的话拼图块初始位置就是答案。
	sliderMinX = pieceSize + 20
	sliderMaxX = bgWidth - pieceSize - 10
	// sliderTolerance 是允许的误差。
	//
	// 太小会让正常人反复失败（鼠标精度就那样），太大等于没验。
	// 5 像素是常见取值。
	sliderTolerance = 5
)

// renderSlider 画出背景图与拼图块，返回两个 data URI。
//
// **图是每次现画的**，不是从一堆预置图里挑：预置图的缺口位置有限，
// 攻击者把它们全存下来就能直接查表。
func renderSlider(gapX, gapY int) (bg, piece string, err error) {
	base := drawBackground()

	// 拼图块 = 从背景上抠下来的那一小块
	pieceImg := image.NewNRGBA(image.Rect(0, 0, pieceSize, pieceSize))
	for y := 0; y < pieceSize; y++ {
		for x := 0; x < pieceSize; x++ {
			if !inPiece(x, y) {
				continue
			}
			pieceImg.Set(x, y, base.At(gapX+x, gapY+y))
		}
	}

	// 背景上把那一块挖掉（压暗 + 描边），用户要把块拖回这里
	for y := 0; y < pieceSize; y++ {
		for x := 0; x < pieceSize; x++ {
			if !inPiece(x, y) {
				continue
			}
			c := color.NRGBA{R: 20, G: 22, B: 26, A: 190}
			if onPieceEdge(x, y) {
				c = color.NRGBA{R: 245, G: 245, B: 245, A: 220}
			}
			base.Set(gapX+x, gapY+y, blend(base.NRGBAAt(gapX+x, gapY+y), c))
		}
	}

	if bg, err = encode(base); err != nil {
		return "", "", err
	}
	if piece, err = encode(pieceImg); err != nil {
		return "", "", err
	}
	return bg, piece, nil
}

// drawBackground 画一张随机的背景。
//
// 用平滑的彩色噪声而不是纯色块：纯色背景上，被挖掉那一块的边界靠简单的
// 边缘检测就能找出来——那样滑块连「看得见的验证」这点作用都没了。
func drawBackground() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, bgWidth, bgHeight))

	seed := make([]byte, 8)
	_, _ = rand.Read(seed)
	fx := 0.02 + float64(seed[0])/6000
	fy := 0.02 + float64(seed[1])/6000
	px := float64(seed[2])
	py := float64(seed[3])
	hueShift := float64(seed[4]) / 255 * 360

	for y := 0; y < bgHeight; y++ {
		for x := 0; x < bgWidth; x++ {
			v := math.Sin(float64(x)*fx+px) * math.Cos(float64(y)*fy+py)
			v += 0.5 * math.Sin(float64(x+y)*fx*1.7+px)
			// v ∈ [-1.5, 1.5] → 色相
			h := math.Mod(hueShift+(v+1.5)/3*120, 360)
			img.Set(x, y, hsl(h, 0.45, 0.55))
		}
	}
	return img
}

// inPiece 判断一个点是否属于拼图块。
//
// 形状是「圆角方块 + 一个凸起」——纯方块的缺口太容易被模板匹配找到。
func inPiece(x, y int) bool {
	const r = 8
	fx, fy := float64(x), float64(y)

	// 凸起：右侧中部的一个半圆
	cx, cy := float64(pieceSize)-2, float64(pieceSize)/2
	if math.Hypot(fx-cx, fy-cy) <= 9 {
		return true
	}
	// 主体：圆角方块（留出右侧凸起的空间）
	w := float64(pieceSize) - 8
	if fx < 0 || fy < 0 || fx >= w || fy >= float64(pieceSize) {
		return false
	}
	// 四角倒圆
	for _, c := range [][2]float64{{r, r}, {w - r, r}, {r, float64(pieceSize) - r}, {w - r, float64(pieceSize) - r}} {
		dx, dy := fx-c[0], fy-c[1]
		outX := (c[0] == r && fx < r) || (c[0] == w-r && fx > w-r)
		outY := (c[1] == r && fy < r) || (c[1] != r && fy > float64(pieceSize)-r)
		if outX && outY && math.Hypot(dx, dy) > r {
			return false
		}
	}
	return true
}

// onPieceEdge 判断是不是缺口的边缘，用来描一圈亮边。
func onPieceEdge(x, y int) bool {
	if !inPiece(x, y) {
		return false
	}
	for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
		if !inPiece(x+d[0], y+d[1]) {
			return true
		}
	}
	return false
}

func blend(dst, src color.NRGBA) color.NRGBA {
	a := float64(src.A) / 255
	return color.NRGBA{
		R: uint8(float64(src.R)*a + float64(dst.R)*(1-a)),
		G: uint8(float64(src.G)*a + float64(dst.G)*(1-a)),
		B: uint8(float64(src.B)*a + float64(dst.B)*(1-a)),
		A: 255,
	}
}

// hsl 把 HSL 转成 RGB。
func hsl(h, s, l float64) color.NRGBA {
	c := (1 - math.Abs(2*l-1)) * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := l - c/2
	var r, g, b float64
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return color.NRGBA{
		R: uint8((r + m) * 255), G: uint8((g + m) * 255),
		B: uint8((b + m) * 255), A: 255,
	}
}

func encode(img image.Image) (string, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", fmt.Errorf("encoding PNG: %w", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
