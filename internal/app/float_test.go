package app

import (
	"fmt"
	"image/color"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"netease-music-mygo/internal/netease"
)

// The mini window seeks where its bar is dragged, and sets the volume where its
// slider is clicked: neither goes to the window, which drags from elsewhere.
func TestMiniSeeksAndSetsVolume(t *testing.T) {
	a := newTestApp()
	a.settings = defaultSettings()
	a.volume = 0.8
	a.pl.Cue([]netease.Track{{ID: 7, Name: "宝船", Duration: 200000}})
	tt := ui.NewTester(a.miniView, miniW, miniH)
	tt.Frame()

	const y = miniH - 12
	left, width := float32(18), float32(miniW-36)
	tt.Press(left+width/4, y)
	tt.Frame()
	tt.Move(left+width*3/4, y)
	tt.Frame()
	tt.Release(left+width*3/4, y)
	tt.Frame()
	if got := a.pl.Position(); got < 140*time.Second || got > 160*time.Second {
		t.Errorf("dragged to three quarters of 200s, at %v", got)
	}

	r, ok := tt.Find("音量")
	if !ok {
		t.Fatal("no volume slider")
	}
	tt.ClickAt(r.X+r.W/4, r.Y+r.H/2)
	tt.Frame()
	if a.volume < 0.15 || a.volume > 0.35 {
		t.Errorf("clicked a quarter along, volume %v", a.volume)
	}
}

// The mini-window button sits beside Settings in the top bar at any width,
// rather than down in the player bar, and opens the mini window when clicked.
func TestTopBarMiniButton(t *testing.T) {
	orig := onShowMini
	defer func() { onShowMini = orig }()

	for _, width := range []int{880, 1180} {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			a := playerBarLayoutApp()
			opened := 0
			onShowMini = func(*app) { opened++ }
			tt := playerBarTester(a, width)

			mini := barElement(t, tt, "迷你窗口")
			settings := barElement(t, tt, "设置")
			if mini.Y > topBarHeight || mini.X+mini.W > settings.X {
				t.Fatalf("mini button not left of settings in top bar: mini=%+v settings=%+v", mini, settings)
			}
			if err := tt.Click("迷你窗口"); err != nil {
				t.Fatal(err)
			}
			if opened != 1 {
				t.Fatalf("clicked top-bar mini button, opened=%d", opened)
			}
		})
	}
}

// Hovering the mini-window cover dims it with the expand prompt, and clicking
// it restores the main window across strip, standard and card layouts.
func TestMiniCoverHoverAndRestore(t *testing.T) {
	orig := onCloseMini
	defer func() { onCloseMini = orig }()

	for _, tc := range []struct {
		name string
		w, h int
	}{
		{"strip", 420, 80},
		{"standard", miniW, miniH},
		{"card", 480, 220},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp()
			a.settings = defaultSettings()
			a.art.add("mini-cover-art", &artwork{bmp: ui.NewBitmap(coverTestImage()), used: time.Now(), size: 1})
			a.pl.Cue([]netease.Track{{ID: 7, Name: "Flower of Life", Album: netease.Album{PicURL: "mini-cover-art"}, Duration: 200000}})

			restored := 0
			onCloseMini = func(*app) { restored++ }

			tt := ui.NewTester(a.miniView, tc.w, tc.h)
			tt.SetPreferences(ui.Preferences{ReduceMotion: true})
			tt.Advance(time.Second)
			tt.Frame()

			cover := barElement(t, tt, "迷你窗口封面")
			imgBefore := tt.Image()
			scale := float32(imgBefore.Bounds().Dx()) / float32(tc.w)
			// Sample off-center inside the cover so the centered expand icon does not skew the scrim check.
			sx := int((cover.X + cover.W*0.25) * scale)
			sy := int((cover.Y + cover.H*0.5) * scale)
			before := color.RGBAModel.Convert(imgBefore.At(sx, sy)).(color.RGBA)

			tt.Move(cover.X+cover.W/2, cover.Y+cover.H/2)
			tt.Frame()
			imgAfter := tt.Image()
			after := color.RGBAModel.Convert(imgAfter.At(sx, sy)).(color.RGBA)
			if after.R >= before.R || after.G >= before.G || after.B >= before.B {
				t.Fatalf("cover hover did not darken scrim: before=%+v after=%+v", before, after)
			}

			if err := tt.Click("迷你窗口封面"); err != nil {
				t.Fatal(err)
			}
			if restored != 1 {
				t.Fatalf("clicked mini cover, restored=%d", restored)
			}
		})
	}
}
