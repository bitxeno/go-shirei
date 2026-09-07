//go:build darwin && !ios

package shirei

// CoreText integration for macOS (Phase 1 + Phase 2).
//
// Split of responsibilities (same as GPUI/Zed's macOS text path):
//   - Phase 1: glyph ink rasterization. rasterizeGlyphCoreText draws one
//     outline glyph into an 8-bit gray bitmap whose bytes ARE the GlyphBM
//     Alpha coverage mask. Layout, shaping, compositing are untouched.
//   - Phase 2: fallback face selection. fallbackScanCoreText resolves which
//     face covers a rune via CTFontCopyDefaultCascadeListForLanguages, then
//     registers that face back into the existing face registry so HarfBuzz
//     shaping and the glyph cache keep working unchanged.
//
// What stays pure Go: shaping (HarfBuzz), bidi, line breaking, color-bitmap
// emoji stamps (sbix/CBDT PNG decode), the Go outline rasterizer (memory-
// backed UseFontBytes faces, CT failures, and all non-darwin platforms).
// Emoji runes keep the Go bucket path (bucketEmoji priority + sbix stamps).

/*
#cgo LDFLAGS: -framework CoreText -framework CoreGraphics -framework CoreFoundation
#include <CoreText/CoreText.h>
#include <CoreGraphics/CoreGraphics.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>
#include <limits.h>

static void ct_release(void *cf) {
    CFRelease((CFTypeRef)cf);
}

static void *ct_retain(void *cf) {
    if (cf) CFRetain((CFTypeRef)cf);
    return cf;
}

// --- Phase 1: glyph rasterization -------------------------------------------

// Bounds of one glyph, in font user space (y-up, pen origin at baseline).
// Returns 1 and fills *out{Left,Bottom,Right,Top}. Empty rects (whitespace)
// come back 1 with zero size; returns 0 only on failure.
static int ct_glyph_bounds(void *font, unsigned gid,
                           float *outLeft, float *outBottom,
                           float *outRight, float *outTop) {
    CGGlyph g = (CGGlyph)gid;
    CGRect r = CTFontGetBoundingRectsForGlyphs((CTFontRef)font, kCTFontOrientationDefault, &g, NULL, 1);
    if (CGRectIsNull(r)) return 0;
    *outLeft = (float)r.origin.x;
    *outBottom = (float)r.origin.y;
    *outRight = (float)(r.origin.x + r.size.width);
    *outTop = (float)(r.origin.y + r.size.height);
    return 1;
}

// Draw one glyph into buf (8bpp gray, stride w, rows top-down). The bitmap
// context stores row 0 as the top of the image while its user space is y-up,
// so translating by (-left, -bottom) places ink at top-down buffer rows with
// row 0 == ink top, matching GlyphBM's y-down layout. Returns 1 on success.
static int ct_draw_glyph(void *font, unsigned gid, unsigned char *buf,
                         int w, int h, float left, float bottom) {
    CGColorSpaceRef cs = CGColorSpaceCreateDeviceGray();
    CGContextRef ctx = CGBitmapContextCreate(buf, w, h, 8, w, cs, kCGImageAlphaNone);
    CGColorSpaceRelease(cs);
    if (!ctx) return 0;
    CGContextSetRGBFillColor(ctx, 1, 1, 1, 1);
    CGContextSetTextMatrix(ctx, CGAffineTransformIdentity);
    CGContextSetAllowsFontSmoothing(ctx, true);
    CGContextSetShouldSmoothFonts(ctx, true);
    CGContextSetShouldAntialias(ctx, true);
    CGContextTranslateCTM(ctx, -left, -bottom);
    CGGlyph g = (CGGlyph)gid;
    CGPoint pos = CGPointMake(0, 0);
    CTFontDrawGlyphs((CTFontRef)font, &g, &pos, 1, ctx);
    CGContextRelease(ctx);
    return 1;
}

// Sized copy of a live instance (used for hidden faces whose file has no
// CT descriptor). Shares the instance's font data.
static void *ct_font_copy_with_size(void *font, float size) {
    return (void *)CTFontCreateCopyWithAttributes((CTFontRef)font, size, NULL, NULL);
}

// CTFont for one (path, collection index) at the given point size. The
// descriptor is selected by the caller via PostScript-name matching
// (descriptor order and loader order can differ); size == raster pixels
// because the glyph cache already folds the backing scale into Px.
static void *ct_font_from_path(const char *path, int index, float size) {
    CFURLRef url = CFURLCreateFromFileSystemRepresentation(kCFAllocatorDefault,
        (const UInt8 *)path, (CFIndex)strlen(path), false);
    if (!url) return NULL;
    CFArrayRef descs = CTFontManagerCreateFontDescriptorsFromURL(url);
    CFRelease(url);
    if (!descs) return NULL;
    CFIndex n = CFArrayGetCount(descs);
    if (index < 0 || index >= n) {
        CFRelease(descs);
        return NULL;
    }
    CTFontDescriptorRef d = (CTFontDescriptorRef)CFArrayGetValueAtIndex(descs, index);
    CTFontRef f = CTFontCreateWithFontDescriptor(d, size, NULL);
    CFRelease(descs);
    return (void *)f; // NULL index refuses instead of drawing the wrong face
}

// --- Phase 2: cascade list --------------------------------------------------

static void *ct_preferred_languages(void) {
    return (void *)CFLocaleCopyPreferredLanguages();
}

// System UI font base with the requested traits, so the cascade items match
// the aspect (bold text cascades to the bold CJK variant, italic to italic).
static void *ct_cascade_base_font(float weightTrait, int italic) {
    CTFontRef base = CTFontCreateUIFontForLanguage(kCTFontUIFontSystem, 12.0, NULL);
    if (!base) base = CTFontCreateWithName(CFSTR("Helvetica"), 12.0, NULL);
    if (!base) return NULL;
    CGFloat w = (CGFloat)weightTrait;
    CFNumberRef wnum = CFNumberCreate(kCFAllocatorDefault, kCFNumberCGFloatType, &w);
    CTFontSymbolicTraits sym = italic ? kCTFontItalicTrait : 0;
    CFNumberRef snum = CFNumberCreate(kCFAllocatorDefault, kCFNumberSInt32Type, &sym);
    const void *keys[2] = { kCTFontWeightTrait, kCTFontSymbolicTrait };
    const void *vals[2] = { wnum, snum };
    CFDictionaryRef traits = CFDictionaryCreate(kCFAllocatorDefault, keys, vals, 2,
        &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
    // Traits live nested under kCTFontTraitsAttribute in a descriptor.
    const void *akeys[1] = { kCTFontTraitsAttribute };
    const void *avals[1] = { traits };
    CFDictionaryRef attrs = CFDictionaryCreate(kCFAllocatorDefault, akeys, avals, 1,
        &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
    // Merge via the descriptor copy API (CTFontCreateCopyWithAttributes does
    // not merge a traits-only descriptor).
    CTFontDescriptorRef baseDesc = CTFontCopyFontDescriptor(base);
    CTFontDescriptorRef merged = CTFontDescriptorCreateCopyWithAttributes(baseDesc, attrs);
    CTFontRef styled = CTFontCreateWithFontDescriptor(merged, 12.0, NULL);
    CFRelease(merged);
    CFRelease(attrs);
    CFRelease(baseDesc);
    CFRelease(traits);
    CFRelease(wnum);
    CFRelease(snum);
    CFRelease(base);
    return (void *)styled;
}

// Retained cascade array (borrowed items stay valid while it lives).
static void *ct_cascade_for(void *font, const void *langs) {
    return (void *)CTFontCopyDefaultCascadeListForLanguages((CTFontRef)font, (CFArrayRef)langs);
}

static int ct_cascade_count(const void *cascade) {
    return cascade ? (int)CFArrayGetCount((CFArrayRef)cascade) : 0;
}

// Copy one table's bytes into a Go buffer. Returns byte count, -1 on missing
// table, -2 when outCap is too small.
static int ct_font_table_copy(void *font, unsigned tag, unsigned char *out, long outCap) {
    CFDataRef data = CTFontCopyTable((CTFontRef)font, (CTFontTableTag)tag, kCTFontTableOptionNoOptions);
    if (!data) return -1;
    long n = (long)CFDataGetLength(data);
    if (n > outCap) {
        CFRelease(data);
        return -2;
    }
    memcpy(out, CFDataGetBytePtr(data), (size_t)n);
    CFRelease(data);
    return (int)n;
}

// Instantiate one cascade entry. The cascade list holds font DESCRIPTORS
// (CTFontDescriptorRef), not fonts — each must be realized before any font
// API touches it. Returns a +1 CTFontRef the caller releases (size is
// irrelevant: only cmap probing and identity are read off it), or NULL.
static void *ct_cascade_font_at(const void *cascade, int i) {
    const void *d = CFArrayGetValueAtIndex((CFArrayRef)cascade, (CFIndex)i);
    if (!d || CFGetTypeID(d) != CTFontDescriptorGetTypeID()) return NULL;
    return (void *)CTFontCreateWithFontDescriptor((CTFontDescriptorRef)d, 0.0, NULL);
}

// cmap coverage probe for one rune (handles astral pairs via UTF-16).
static int ct_font_has_glyph(void *font, unsigned ch, unsigned *outGid) {
    UniChar buf[2];
    CFIndex n = 1;
    if (ch >= 0x10000UL) {
        unsigned v = ch - 0x10000UL;
        buf[0] = (UniChar)(0xD800 | (v >> 10));
        buf[1] = (UniChar)(0xDC00 | (v & 0x3FF));
        n = 2;
    } else {
        buf[0] = (UniChar)ch;
    }
    CGGlyph g = 0;
    if (!CTFontGetGlyphsForCharacters((CTFontRef)font, buf, &g, n)) return 0;
    if (g == 0) return 0;
    *outGid = (unsigned)g;
    return 1;
}

// --- face identity (cascade -> registry mapping) ----------------------------

static char *ct_copy_cfstring(CFStringRef s) {
    if (!s) return NULL;
    CFIndex len = CFStringGetLength(s);
    CFIndex max = CFStringGetMaximumSizeForEncoding(len, kCFStringEncodingUTF8) + 1;
    char *buf = (char *)malloc((size_t)max);
    if (!buf) return NULL;
    if (!CFStringGetCString(s, buf, max, kCFStringEncodingUTF8)) {
        free(buf);
        return NULL;
    }
    return buf;
}

static char *ct_font_psname(void *font) {
    return ct_copy_cfstring(CTFontCopyPostScriptName((CTFontRef)font));
}

static char *ct_font_family(void *font) {
    return ct_copy_cfstring(CTFontCopyFamilyName((CTFontRef)font));
}

static char *ct_font_path(void *font) {
    CFURLRef url = (CFURLRef)CTFontCopyAttribute((CTFontRef)font, kCTFontURLAttribute);
    if (!url) return NULL;
    UInt8 buf[PATH_MAX];
    char *out = NULL;
    if (CFURLGetFileSystemRepresentation(url, true, buf, sizeof(buf))) {
        out = strdup((const char *)buf);
    }
    CFRelease(url);
    return out;
}

static unsigned ct_font_traits(void *font) {
    return (unsigned)CTFontGetSymbolicTraits((CTFontRef)font);
}

static float ct_font_weight(void *font) {
    CFTypeRef v = CTFontCopyAttribute((CTFontRef)font, kCTFontWeightTrait);
    float w = 0;
    if (v) {
        if (CFGetTypeID(v) == CFNumberGetTypeID()) {
            CGFloat d = 0;
            if (CFNumberGetValue((CFNumberRef)v, kCFNumberCGFloatType, &d)) w = (float)d;
        }
        CFRelease(v);
    }
    return w;
}
*/
import "C"

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/go-text/typesetting/font/opentype"
	"github.com/go-text/typesetting/font/opentype/tables"
)

func init() {
	rasterizeGlyphPlatform = rasterizeGlyphCoreText
	fallbackScanPlatform = fallbackScanCoreText
}

func ctGoString(p *C.char) string {
	if p == nil {
		return ""
	}
	s := C.GoString(p)
	C.free(unsafe.Pointer(p))
	return s
}

// ---------------------------------------------------------------------------
// Phase 1: CoreText glyph rasterization
// ---------------------------------------------------------------------------
// The glyph-cache key already folds the backing scale into Px, so one CTFont
// per (FontId, Px) at size Px rasterizes straight into device pixels.
type ctSizedKey struct {
	fid FontId
	px  uint16
}

const ctSizedCap = 4096

var (
	ctMu         sync.Mutex
	ctSizedFonts = map[ctSizedKey]unsafe.Pointer{}

	// ctHiddenInst retains one live cascade instance per hidden registered
	// face. File-backed CT descriptors do not cover every face (e.g. the
	// Display variants in PingFangUI.ttc), so these faces rasterize from
	// sized copies of the alias instance instead. Bounded by hidden face
	// count; instances are never released.
	ctHiddenInst   = map[FontId]unsafe.Pointer{}
	ctHiddenVerify = map[FontId]bool{}
)

// ctFontFor returns the CTFont for a registered (face, device size) pair.
// Faces without a file (UseFontBytes) keep the Go rasterizer. The CT
// descriptor is selected by PostScript name: descriptor order and the
// registry's loader order can differ, so face.index must never be used
// positionally here.
func ctFontFor(fid FontId, px uint16) unsafe.Pointer {
	ctMu.Lock()
	defer ctMu.Unlock()
	if f, ok := ctSizedFonts[ctSizedKey{fid, px}]; ok {
		return f
	}
	face := GetFace(fid)
	if inst := ctHiddenInst[face.FontId]; inst != nil {
		if f := C.ct_font_copy_with_size(inst, C.float(px)); f != nil {
			return ctCacheSized(fid, px, f)
		}
	}
	if face.Filepath == "" {
		return nil
	}
	faces := describeFacesCached(face.Filepath)
	var ps string
	for i := range faces {
		if faces[i].index == face.index {
			ps = tsPSNamesCached(face.Filepath, faces)[i]
			break
		}
	}
	if ps == "" {
		return nil
	}
	descIdx := ctDescIndexForPS(face.Filepath, ps, faces)
	if descIdx < 0 {
		return nil
	}
	cp := C.CString(face.Filepath)
	f := C.ct_font_from_path(cp, C.int(descIdx), C.float(px))
	C.free(unsafe.Pointer(cp))
	if f == nil {
		return nil
	}
	return ctCacheSized(fid, px, f)
}

// ctCacheSized inserts a +1 CTFont into the sized cache (caller holds ctMu).
func ctCacheSized(fid FontId, px uint16, f unsafe.Pointer) unsafe.Pointer {
	if len(ctSizedFonts) >= ctSizedCap {
		for k := range ctSizedFonts {
			C.ct_release(unsafe.Pointer(ctSizedFonts[k]))
			delete(ctSizedFonts, k)
		}
	}
	ctSizedFonts[ctSizedKey{fid, px}] = f
	return f
}

// rasterizeGlyphCoreText draws one glyph's ink into an A8 buffer whose row 0
// is the ink top (same contract as the Go outline path: OffX=left,
// OffY=-top). Returns ok=false for the Go rasterizer to try (no file, CT
// failure). Empty ink (space) is handled with an empty GlyphBM.
func rasterizeGlyphCoreText(key GlyphKey) (GlyphBM, bool) {
	f := ctFontFor(key.FontId, key.Px)
	if f == nil {
		return GlyphBM{}, false
	}
	var l, b, r, t C.float
	if C.ct_glyph_bounds(f, C.uint(key.GlyphId), &l, &b, &r, &t) == 0 {
		return GlyphBM{}, false
	}
	left, bottom, right, top := float32(l), float32(b), float32(r), float32(t)
	if !(right > left) || !(top > bottom) {
		return GlyphBM{}, true
	}
	// 1px pad like the Go path so anti-aliasing is never clipped.
	fl := float32(math.Floor(float64(left))) - 1
	fr := float32(math.Ceil(float64(right))) + 1
	ft := float32(math.Ceil(float64(top))) + 1
	fb := float32(math.Floor(float64(bottom))) - 1
	w := int(fr - fl)
	h := int(ft - fb)
	if w <= 0 || h <= 0 || int64(w)*int64(h) > 64<<20 {
		return GlyphBM{}, true
	}
	pix := make([]byte, w*h)
	if C.ct_draw_glyph(f, C.uint(key.GlyphId), (*C.uchar)(unsafe.Pointer(&pix[0])),
		C.int(w), C.int(h), C.float(fl), C.float(fb)) == 0 {
		return GlyphBM{}, false
	}
	return GlyphBM{
		W:      w,
		H:      h,
		OffX:   fl,
		OffY:   -ft,
		Alpha:  pix,
		Stride: w,
	}, true
}

// ---------------------------------------------------------------------------
// Phase 2: cascade-list fallback
// ---------------------------------------------------------------------------
var (
	ctLangsOnce sync.Once
	ctLangs     unsafe.Pointer

	ctCascadeMu    sync.Mutex
	ctCascadeCache = map[FontAspect]unsafe.Pointer{}

	// per-path caches for mapping cascade fonts back to registry faces
	ctRegMu     sync.Mutex
	ctDescribed = map[string][]describedFace{}
	ctPSNames   = map[string][]string{} // CT descriptor order (for raster font selection)
	ctTSPSNames = map[string][]string{} // typesetting loader order (for registry matching)
)

func ctPreferredLanguages() unsafe.Pointer {
	ctLangsOnce.Do(func() {
		ctLangs = C.ct_preferred_languages()
	})
	return ctLangs
}

// ctCascadeForAspect is the default cascade list for the requested aspect,
// computed once per aspect from the system UI font + the user's preferred
// languages (so Han falls to PingFang SC/TC by locale, Kana to Hiragino).
func ctCascadeForAspect(a FontAspect) unsafe.Pointer {
	ctCascadeMu.Lock()
	defer ctCascadeMu.Unlock()
	if c, ok := ctCascadeCache[a]; ok {
		return c
	}
	langs := ctPreferredLanguages()
	if langs == nil {
		return nil
	}
	base := C.ct_cascade_base_font(C.float(ctWeightTrait(a.Weight)), C.int(b2i(a.Style == StyleItalic)))
	if base == nil {
		return nil
	}
	defer C.ct_release(base)
	c := C.ct_cascade_for(base, langs)
	ctCascadeCache[a] = c
	return c
}

// ctFilePSName reads the name-table PostScript name (ID 6): the backing
// file identity of hidden UI aliases.
func ctFilePSName(f unsafe.Pointer) string {
	buf := make([]byte, 1<<20)
	n := int(C.ct_font_table_copy(f, 0x6e616d65, (*C.uchar)(unsafe.Pointer(&buf[0])), C.long(len(buf))))
	if n < 0 {
		return ""
	}
	nm, _, err := tables.ParseName(buf[:n])
	if err != nil {
		return ""
	}
	return nm.Name(6)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ctBaseWeightDebug reports the weight trait of the styled cascade base.
// Diagnostic helper.
func ctBaseWeightDebug(weightTrait float32, italic bool) float32 {
	base := C.ct_cascade_base_font(C.float(weightTrait), C.int(b2i(italic)))
	if base == nil {
		return -99
	}
	defer C.ct_release(base)
	return float32(C.ct_font_weight(base))
}

// ctCascadeHitPS returns the CT-reported and name-table PostScript names of
// the first cascade entry covering ch. Diagnostic helper (tests/demos).
func ctCascadeHitPS(ch rune, a FontAspect) (ps, filePS string) {
	cascade := ctCascadeForAspect(a)
	if cascade == nil {
		return "", ""
	}
	n := int(C.ct_cascade_count(cascade))
	for i := 0; i < n; i++ {
		f := C.ct_cascade_font_at(cascade, C.int(i))
		if f == nil {
			continue
		}
		var gid C.uint
		hit := C.ct_font_has_glyph(f, C.uint(ch), &gid) != 0
		var p1, p2 string
		if hit {
			p1, p2 = ctPSNameOf(f), ctFilePSName(f)
		}
		C.ct_release(f)
		if hit {
			return p1, p2
		}
	}
	return "", ""
}

// fallbackScanCoreText resolves ch through the system cascade list. The
// returned GlyphId is the cascade font's coverage probe; shaping re-runs
// HarfBuzz on the registered face for the final GIDs.
func fallbackScanCoreText(ch rune, aspect FontAspect) (FontId, GlyphId, bool) {
	if isEmojiRune(ch) {
		// Emoji keeps the Go path (bucket priority + sbix stamps).
		return 0, 0, false
	}
	cascade := ctCascadeForAspect(aspect)
	if cascade == nil {
		return 0, 0, false
	}
	n := int(C.ct_cascade_count(cascade))
	for i := 0; i < n; i++ {
		f := C.ct_cascade_font_at(cascade, C.int(i))
		if f == nil {
			continue
		}
		var gid C.uint
		hit := C.ct_font_has_glyph(f, C.uint(ch), &gid) != 0
		var fid FontId
		if hit {
			fid = registerCascadeFace(f, ch, aspect)
			if fid != 0 && GetFace(fid).colorPaintOnly {
				fid = 0
			}
		}
		C.ct_release(f)
		if fid != 0 {
			return fid, GlyphId(gid), true
		}
	}
	return 0, 0, false
}

// ctHiddenAliasFamilies maps hidden system-UI alias prefixes (PostScript
// names of cascade fonts without a file URL) to scan-registered real
// families, in preference order. The alias fonts are subsets of these
// families' data, so shaping the real face covers whatever the alias
// covered (verified per rune by LookupGlyph below).
var ctHiddenAliasFamilies = []struct {
	prefix string
	fams   []string
}{
	{"AppleSimplifiedChineseFont", []string{"PingFang SC"}},
	{"PingFangUIDisplaySC", []string{"PingFang SC"}},
	{"PingFangUITextSC", []string{"PingFang SC"}},
	{"AppleTraditionalChineseFont", []string{"PingFang TC"}},
	{"PingFangUIDisplayTC", []string{"PingFang TC"}},
	{"PingFangUITextTC", []string{"PingFang TC"}},
	{"AppleHongKongChineseFont", []string{"PingFang HK"}},
	{"PingFangUIDisplayHK", []string{"PingFang HK"}},
	{"AppleMacaoChineseFont", []string{"PingFang HK", "PingFang TC"}},
	{"AppleJapaneseFont", []string{"Hiragino Sans", "Hiragino Kaku Gothic ProN"}},
	{"HiraKakuInterface", []string{"Hiragino Sans", "Hiragino Kaku Gothic ProN"}},
	{"HiraginoKakuGothicInterface", []string{"Hiragino Sans", "Hiragino Kaku Gothic ProN"}},
	{"AppleKoreanFont", []string{"Apple SD Gothic Neo"}},
	{"AppleSDGothicNeoI", []string{"Apple SD Gothic Neo"}},
	{"AppleSymbolsFB", []string{"Apple Symbols"}},
}

func ctAliasFamilies(psName string) []string {
	for _, e := range ctHiddenAliasFamilies {
		if strings.HasPrefix(psName, e.prefix) || strings.HasPrefix(psName, "."+e.prefix) {
			return e.fams
		}
	}
	return nil
}

// registerCascadeFace maps one covering cascade CTFont to a registered FontId.
// File-backed fonts publish their file's faces (idempotent with the system
// scan) and pick the exact face by PostScript name. Hidden UI aliases (no
// file URL) first try the hidden backing files by PostScript name (exact
// face identity, e.g. .PingFangUIDisplaySC-Default in PingFangUI.ttc), then
// the alias->family map with per-rune coverage verification, so a subset
// alias can never select a face that shapes ch as tofu.
func registerCascadeFace(f unsafe.Pointer, ch rune, aspect FontAspect) FontId {
	psName := ctPSNameOf(f)
	if psName == "" {
		return 0
	}
	if path := ctPathOf(f); path != "" {
		if fid := matchPSInFile(path, psName); fid != 0 {
			// Parse now (metrics for caret/descender callers): the old
			// cmap-probe path published the parse at selection time.
			GetParsedFont(fid)
			return fid
		}
		// Same file, looser identity: CT-reported (family, aspect), then
		// any face of the file with the cascade font's aspect.
		asp := ctAspectOf(f)
		if fid := LookupFace(FaceLookupKey{ctFamilyOf(f), asp}); fid != 0 {
			GetParsedFont(fid)
			return fid
		}
		for _, d := range describeFacesCached(path) {
			if d.key.Aspect == asp {
				if fid := LookupFace(d.key); fid != 0 {
					GetParsedFont(fid)
					return fid
				}
			}
		}
		return 0
	}
	for _, hp := range ctHiddenBackingFiles() {
		fid := matchPSInFile(hp, psName)
		if fid == 0 || LookupGlyph(fid, ch) == 0 {
			continue
		}
		if afid := adoptHiddenFace(fid, aspect, f, ch); afid != 0 {
			return afid
		}
	}
	// The CT-reported PS name of a hidden alias is the wrapper identity;
	// the name table carries the backing file's PostScript name.
	if filePS := ctFilePSName(f); filePS != "" && filePS != psName {
		for _, hp := range ctHiddenBackingFiles() {
			fid := matchPSInFile(hp, filePS)
			if fid == 0 || LookupGlyph(fid, ch) == 0 {
				continue
			}
			if afid := adoptHiddenFace(fid, aspect, f, ch); afid != 0 {
				return afid
			}
		}
	}
	asp := ctAspectOf(f)
	for _, fam := range ctAliasFamilies(psName) {
		if fid := LookupFace(FaceLookupKey{fam, asp}); fid != 0 && LookupGlyph(fid, ch) != 0 {
			return fid
		}
		// Wrong weight but right family beats tofu (e.g. upright CJK for
		// an italic request — PingFang has no italics).
		if asp != DefaultFontAspect() {
			if fid := LookupFace(FaceLookupKey{fam, DefaultFontAspect()}); fid != 0 && LookupGlyph(fid, ch) != 0 {
				return fid
			}
		}
	}
	return 0
}

// ctHiddenBackingFiles are font files backing hidden system-UI aliases
// (no URL attribute): the FontServices Reserved directory (PingFangUI.ttc
// on modern macOS) plus legacy locations. Existence-checked once.
var (
	ctHiddenFilesOnce sync.Once
	ctHiddenFiles     []string
)

func ctHiddenBackingFiles() []string {
	ctHiddenFilesOnce.Do(func() {
		seen := map[string]bool{}
		add := func(p string) {
			if seen[p] {
				return
			}
			seen[p] = true
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				ctHiddenFiles = append(ctHiddenFiles, p)
			}
		}
		if ms, err := filepath.Glob("/System/Library/PrivateFrameworks/FontServices.framework/Versions/A/Resources/Reserved/*"); err == nil {
			for _, m := range ms {
				if isFontFilePath(m) {
					add(m)
				}
			}
		}
		add("/System/Library/Fonts/PingFang.ttc") // pre-Sequoia location
		// Backing files of the hidden CJK UI aliases (.HiraKakuInterface-*,
		// .AppleSDGothicNeoI-*): matched by name-table PostScript name.
		add("/System/Library/Fonts/ヒラギノ角ゴシック W3.ttc")
		add("/System/Library/Fonts/ヒラギノ角ゴシック W4.ttc")
		add("/System/Library/Fonts/ヒラギノ角ゴシック W6.ttc")
		add("/System/Library/Fonts/Hiragino Sans GB.ttc")
		add("/System/Library/Fonts/AppleSDGothicNeo.ttc")
	})
	return ctHiddenFiles
}

// describeFacesCached describes one file's faces, memoized per path.
func describeFacesCached(path string) []describedFace {
	ctRegMu.Lock()
	defer ctRegMu.Unlock()
	faces, ok := ctDescribed[path]
	if !ok {
		faces = describeFontFile(path)
		ctDescribed[path] = faces
	}
	return faces
}

// ctPSNamesCached returns the CT PostScript name per face index for a file.
func ctPSNamesCached(path string, faces []describedFace) []string {
	ctRegMu.Lock()
	defer ctRegMu.Unlock()
	psns, ok := ctPSNames[path]
	if !ok {
		psns = make([]string, len(faces))
		for i := range faces {
			psns[i] = ctPSNameForIndex(path, faces[i].index)
		}
		ctPSNames[path] = psns
	}
	return psns
}

func bumpFontLookupEpoch() {
	faceRegistryMu.Lock()
	res.fontLookupEpoch++
	faceRegistryMu.Unlock()
}

// tsPSNamesCached returns the name-table PostScript name (ID 6) per face in
// typesetting loader order. CT descriptor order and loader order can differ
// (PingFangUI.ttc), so registry matching must use this side, never the
// CT-side list positionally.
func tsPSNamesCached(path string, faces []describedFace) []string {
	ctRegMu.Lock()
	defer ctRegMu.Unlock()
	if ps, ok := ctTSPSNames[path]; ok {
		return ps
	}
	ps := make([]string, len(faces))
	if f, err := os.Open(path); err == nil {
		func() {
			defer f.Close()
			loaders, err := opentype.NewLoaders(f)
			if err != nil {
				return
			}
			nameTag := opentype.MustNewTag("name")
			for i := range ps {
				if i >= len(loaders) {
					break
				}
				raw, err := loaders[i].RawTable(nameTag)
				if err != nil {
					continue
				}
				nm, _, err := tables.ParseName(raw)
				if err != nil {
					continue
				}
				ps[i] = nm.Name(6)
			}
		}()
	}
	ctTSPSNames[path] = ps
	return ps
}

// ctDescIndexForPS resolves a PostScript name to a CT descriptor index.
func ctDescIndexForPS(path, ps string, faces []describedFace) int {
	for i, p := range ctPSNamesCached(path, faces) {
		if p == ps {
			return i
		}
	}
	return -1
}

// adoptHiddenFace ensures a registry entry for (file family, requested
// aspect) sharing the backing file+index, then adopts the cascade alias
// instance under it for rasterization. Per-aspect entries let Regular and
// Bold requests keep their own CT instances (advances still come from the
// shared file tables — exact for full-width CJK).
func adoptHiddenFace(fileFid FontId, aspect FontAspect, alias unsafe.Pointer, ch rune) FontId {
	afid := ensureHiddenAspectFace(fileFid, aspect)
	if afid == 0 {
		return 0
	}
	if ctAdoptHiddenInstance(afid, alias, ch) {
		return afid
	}
	return 0
}

// ensureHiddenAspectFace returns the FontId for (family, aspect) backed by
// the same file+index as fileFid, publishing an alias entry when needed.
func ensureHiddenAspectFace(fileFid FontId, aspect FontAspect) FontId {
	ff := GetFace(fileFid)
	if ff.FaceLookupKey.Aspect == aspect {
		return fileFid
	}
	key := FaceLookupKey{ff.Family, aspect}
	if fid := LookupFace(key); fid != 0 {
		if gf := GetFace(fid); gf.Filepath == ff.Filepath && gf.index == ff.index {
			return fid
		}
	}
	faceRegistryMu.Lock()
	face := _nextFaceLocked()
	face.Filepath = ff.Filepath
	face.index = ff.index
	face.FaceLookupKey = key
	face.colorPaintOnly = ff.colorPaintOnly
	_mapFaceLocked(key, face.FontId)
	res.fontLookupEpoch++
	fid := face.FontId
	faceRegistryMu.Unlock()
	return fid
}

// ctAdoptHiddenInstance verifies the cascade alias instance and the
// registered file face share a GID space (same underlying data), then
// retains the instance for rasterization. Shaping runs on the file face
// while ink comes from sized copies of the instance, so a mismatch would
// draw wrong glyphs — verification rejects the face instead. One-time per
// face.
func ctAdoptHiddenInstance(fid FontId, alias unsafe.Pointer, ch rune) bool {
	ctMu.Lock()
	defer ctMu.Unlock()
	if ok, done := ctHiddenVerify[fid]; done {
		return ok
	}
	ok := false
	if parsed := GetParsedFont(fid); parsed != nil {
		var agid C.uint
		if C.ct_font_has_glyph(alias, C.uint(ch), &agid) != 0 {
			if fgid, found := parsed.NominalGlyph(ch); found && GlyphId(fgid) == GlyphId(agid) {
				ok = true
				for _, s := range []rune{'永', 'あ', '가', 'A', '0'} {
					var sg C.uint
					if C.ct_font_has_glyph(alias, C.uint(s), &sg) == 0 {
						continue // alias subset need not cover the sample
					}
					fg, found := parsed.NominalGlyph(s)
					if !found || GlyphId(fg) != GlyphId(sg) {
						ok = false
						break
					}
				}
			}
		}
	}
	ctHiddenVerify[fid] = ok
	if ok {
		ctHiddenInst[fid] = C.ct_retain(alias)
	}
	return ok
}

// matchPSInFile publishes a file's faces and returns the one whose
// PostScript name matches (exact face identity), or 0.
func matchPSInFile(path, psName string) FontId {
	faces := describeFacesCached(path)
	if len(faces) == 0 {
		return 0
	}
	// Publish every face of the file so LookupFace can find the sibling we
	// want (publish skips already-mapped keys). Epoch bumps preserve the
	// "new faces invalidate shape keys" contract.
	if n := publishDescribedFaces(faces); n > 0 {
		bumpFontLookupEpoch()
	}
	for i, p := range tsPSNamesCached(path, faces) {
		if p == psName {
			if fid := LookupFace(faces[i].key); fid != 0 {
				return fid
			}
		}
	}
	return 0
}

func ctPSNameOf(f unsafe.Pointer) string {
	return ctGoString(C.ct_font_psname(f))
}

func ctFamilyOf(f unsafe.Pointer) string {
	return ctGoString(C.ct_font_family(f))
}

func ctPathOf(f unsafe.Pointer) string {
	return ctGoString(C.ct_font_path(f))
}

func ctPSNameForIndex(path string, index int) string {
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	f := C.ct_font_from_path(cp, C.int(index), 12)
	if f == nil {
		return ""
	}
	defer C.ct_release(f)
	return ctPSNameOf(f)
}

// ctAspectOf reconstructs a FontAspect from the cascade font's real traits
// (used only for registry mapping, never for shaping).
func ctAspectOf(f unsafe.Pointer) FontAspect {
	traits := uint32(C.ct_font_traits(f))
	style := StyleNormal
	if traits&uint32(C.kCTFontItalicTrait) != 0 {
		style = StyleItalic
	}
	stretch := StretchNormal
	if traits&uint32(C.kCTFontCondensedTrait) != 0 {
		stretch = StretchCondensed
	} else if traits&uint32(C.kCTFontExpandedTrait) != 0 {
		stretch = StretchExpanded
	}
	return FontAspect{
		Weight:  ctWeightFromTrait(float32(C.ct_font_weight(f))),
		Style:   style,
		Stretch: stretch,
	}
}

// ctWeightTrait maps a shirei weight (100-900) to the kCTFontWeightTrait
// scale (-1..1) using Apple's documented anchor values.
func ctWeightTrait(w Weight) float32 {
	switch {
	case w <= 100:
		return -0.8
	case w <= 200:
		return -0.6
	case w <= 300:
		return -0.4
	case w <= 400:
		return 0
	case w <= 500:
		return 0.23
	case w <= 600:
		return 0.3
	case w <= 700:
		return 0.4
	case w <= 800:
		return 0.56
	default:
		return 0.62
	}
}

// ctWeightFromTrait is the inverse of ctWeightTrait for registry mapping.
func ctWeightFromTrait(t float32) Weight {
	switch {
	case t <= -0.7:
		return WeightThin
	case t <= -0.5:
		return WeightExtraLight
	case t <= -0.2:
		return WeightLight
	case t <= 0.11:
		return WeightNormal
	case t <= 0.265:
		return WeightMedium
	case t <= 0.35:
		return WeightSemibold
	case t <= 0.48:
		return WeightBold
	case t <= 0.59:
		return WeightExtraBold
	default:
		return WeightBlack
	}
}
