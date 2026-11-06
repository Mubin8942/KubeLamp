// genicon draws the KubeDeck icon procedurally and writes a macOS .icns
// (plus a 1024px PNG preview). Usage: go run ./tools/genicon -o packaging/macos
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// signed distance to a rounded square (macOS Big Sur style body: 824/1024 with 100px margin)
func sdRoundRect(x, y, half, r float64) float64 {
	dx, dy := math.Abs(x)-(half-r), math.Abs(y)-(half-r)
	return math.Hypot(math.Max(dx, 0), math.Max(dy, 0)) + math.Min(math.Max(dx, dy), 0) - r
}

// signed distance to a pointy-top regular hexagon with circumradius-ish r
func sdHex(x, y, r float64) float64 {
	// iq's hexagon is flat-top; swap axes for pointy-top
	px, py := math.Abs(y), math.Abs(x)
	const kx, ky, kz = -0.866025404, 0.5, 0.577350269
	d := 2 * math.Min(kx*px+ky*py, 0)
	px -= d * kx
	py -= d * ky
	px -= clamp(px, -kz*r, kz*r)
	py -= r
	l := math.Hypot(px, py)
	if py < 0 {
		return -l
	}
	return l
}

func render(size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4
	top := [3]float64{57, 135, 229} // #3987e5
	bot := [3]float64{28, 92, 171}  // #1c5cab
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var body, glyph float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					// normalised coords: -0.5..0.5
					x := (float64(px)+(float64(sx)+0.5)/ss)/float64(size) - 0.5
					y := (float64(py)+(float64(sy)+0.5)/ss)/float64(size) - 0.5
					if sdRoundRect(x, y, 0.4023, 0.0916) > 0 { // 824px body, 185px corner at 1024
						continue
					}
					body++
					ring := math.Abs(sdHex(x, y, 0.205)) - 0.028
					dot := math.Hypot(x, y) - 0.062
					// three spokes from the centre to alternate vertices (wheel / cluster motif)
					spoke := 1.0
					for k := 0; k < 3; k++ {
						a := math.Pi/2 + float64(k)*2*math.Pi/3
						ux, uy := math.Cos(a), -math.Sin(a)
						t := clamp(x*ux+y*uy, 0, 0.205)
						spoke = math.Min(spoke, math.Hypot(x-t*ux, y-t*uy)-0.018)
					}
					if ring <= 0 || dot <= 0 || spoke <= 0 {
						glyph++
					}
				}
			}
			if body == 0 {
				continue
			}
			n := float64(ss * ss)
			t := float64(py) / float64(size)
			r, g, b := top[0]+(bot[0]-top[0])*t, top[1]+(bot[1]-top[1])*t, top[2]+(bot[2]-top[2])*t
			f := glyph / body
			r, g, b = r+(255-r)*f, g+(255-g)*f, b+(255-b)*f
			img.SetNRGBA(px, py, color.NRGBA{uint8(r), uint8(g), uint8(b), uint8(math.Round(body / n * 255))})
		}
	}
	return img
}

func pngBytes(size int) []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, render(size))
	return buf.Bytes()
}

func main() {
	out := flag.String("o", ".", "output directory")
	flag.Parse()
	_ = os.MkdirAll(*out, 0o755)
	// icns entries (all PNG-compressed, supported since 10.7)
	entries := []struct {
		typ  string
		size int
	}{
		{"icp4", 16}, {"icp5", 32}, {"icp6", 64}, {"ic07", 128}, {"ic08", 256}, {"ic09", 512}, {"ic10", 1024},
		{"ic11", 32}, {"ic12", 64}, {"ic13", 256}, {"ic14", 512},
	}
	cache := map[int][]byte{}
	var body bytes.Buffer
	for _, e := range entries {
		if cache[e.size] == nil {
			cache[e.size] = pngBytes(e.size)
		}
		body.WriteString(e.typ)
		binary.Write(&body, binary.BigEndian, uint32(8+len(cache[e.size])))
		body.Write(cache[e.size])
	}
	var icns bytes.Buffer
	icns.WriteString("icns")
	binary.Write(&icns, binary.BigEndian, uint32(8+body.Len()))
	icns.Write(body.Bytes())
	must(os.WriteFile(filepath.Join(*out, "KubeDeck.icns"), icns.Bytes(), 0o644))
	must(os.WriteFile(filepath.Join(*out, "icon-1024.png"), cache[1024], 0o644))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
