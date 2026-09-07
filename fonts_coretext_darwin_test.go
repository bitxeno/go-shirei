//go:build darwin && !ios

package shirei

import (
	"math"
	"testing"
)

// The CoreText rasterizer must honor the same GlyphBM contract as the Go
// outline path: same box (within a pixel), same pen-relative offsets, same
// vertical orientation. Compares both paths on one Latin glyph.
func TestCoreTextParityWithGoRaster(t *testing.T) {
	InitFontSubsystem()
	fid, gid := FallbackFontFor('A', DefaultFontAspect())
	if fid == 0 || gid == 0 {
		t.Fatal("no fallback face for 'A'")
	}
	face := GetFace(fid)
	if face.Filepath == "" {
		t.Skip("fallback face has no file")
	}
	key := GlyphKey{FontId: fid, GlyphId: gid, Px: 48}

	bmCT := rasterizeGlyph(key)
	if len(bmCT.Alpha) == 0 {
		t.Fatalf("CT path produced no mask for %q", face.Family)
	}

	prev := rasterizeGlyphPlatform
	rasterizeGlyphPlatform = nil
	bmGO := rasterizeGlyph(key)
	rasterizeGlyphPlatform = prev
	if len(bmGO.Alpha) == 0 {
		t.Fatalf("Go path produced no mask for %q", face.Family)
	}

	if d := abs(bmCT.W - bmGO.W); d > 2 {
		t.Errorf("width CT=%d GO=%d", bmCT.W, bmGO.W)
	}
	if d := abs(bmCT.H - bmGO.H); d > 2 {
		t.Errorf("height CT=%d GO=%d", bmCT.H, bmGO.H)
	}
	if d := math.Abs(float64(bmCT.OffX - bmGO.OffX)); d > 2 {
		t.Errorf("OffX CT=%.1f GO=%.1f", bmCT.OffX, bmGO.OffX)
	}
	if d := math.Abs(float64(bmCT.OffY - bmGO.OffY)); d > 2 {
		t.Errorf("OffY CT=%.1f GO=%.1f", bmCT.OffY, bmGO.OffY)
	}
	inkCT, inkGO := inkCount(bmCT), inkCount(bmGO)
	if inkCT == 0 || inkGO == 0 {
		t.Fatalf("empty ink CT=%d GO=%d", inkCT, inkGO)
	}
	if r := float64(inkCT) / float64(inkGO); r < 0.7 || r > 1.4 {
		t.Errorf("ink ratio CT/GO = %.2f (%d/%d)", r, inkCT, inkGO)
	}
	// Center of mass Y must agree: catches an upside-down mask.
	if d := math.Abs(comY(bmCT) - comY(bmGO)); d > 2 {
		t.Errorf("center-of-mass Y differs by %.1f px (flipped?)", d)
	}
}

// The CJK pipeline resolves through the cascade to a native face and
// produces CoreText ink.
func TestCoreTextFallbackCJK(t *testing.T) {
	InitFontSubsystem()
	fid, gid := FallbackFontFor('汉', DefaultFontAspect())
	if fid == 0 || gid == 0 {
		t.Fatal("no fallback face for 汉")
	}
	t.Logf("fallback(汉) -> %q %s", GetFace(fid).Family, GetFace(fid).Filepath)
	shaped := ShapeText("汉字", DefaultTextStyle())
	if len(shaped.Lines) == 0 || len(shaped.Lines[0].Segments) == 0 {
		t.Fatal("no segments")
	}
	g := shaped.Lines[0].Segments[0].Glyphs[0]
	bm := rasterizeGlyph(GlyphKey{FontId: g.FontId, GlyphId: g.GlyphId, Px: 32})
	if inkCount(bm) < 20 {
		t.Fatalf("CJK mask nearly empty (%dx%d)", bm.W, bm.H)
	}

	// Bold CJK resolves (possibly to a per-aspect entry) and inks.
	boldStyle := TextStyleWith(DefaultTextStyle(), FontSize(12), FontWeight(WeightBold))
	bshaped := ShapeText("汉", boldStyle)
	if len(bshaped.Lines) == 0 || len(bshaped.Lines[0].Segments) == 0 {
		t.Fatal("no bold segments")
	}
	bg := bshaped.Lines[0].Segments[0].Glyphs[0]
	bbm := rasterizeGlyph(GlyphKey{FontId: bg.FontId, GlyphId: bg.GlyphId, Px: 32})
	t.Logf("bold(汉) -> %q mask=%dx%d ink=%d", GetFace(bg.FontId).Family, bbm.W, bbm.H, inkCount(bbm))
	if inkCount(bbm) < 20 {
		t.Fatalf("bold CJK mask nearly empty (%dx%d)", bbm.W, bbm.H)
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func inkCount(bm GlyphBM) int {
	n := 0
	for _, v := range bm.Alpha {
		if v > 64 {
			n++
		}
	}
	return n
}

func comY(bm GlyphBM) float64 {
	var sum, n float64
	for y := 0; y < bm.H; y++ {
		for x := 0; x < bm.W; x++ {
			if bm.Alpha[y*bm.Stride+x] > 64 {
				sum += float64(y)
				n++
			}
		}
	}
	if n == 0 {
		return 0
	}
	return sum / n
}
