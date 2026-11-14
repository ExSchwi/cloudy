package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"netease-music-mygo/internal/download"
	"netease-music-mygo/internal/local"
	"netease-music-mygo/internal/lyrics"
	"netease-music-mygo/internal/netease"
)

type lyricDownloadState struct {
	open, saving bool
	song         netease.Track
	timestamps   bool
	translations bool
	format       string
}

func (a *app) openLyricDownload(song netease.Track) {
	if a.lyricDownload.saving {
		a.toast = "正在保存歌词，请稍候"
		return
	}
	a.lyricDownload = lyricDownloadState{open: true, song: song, timestamps: true, translations: true, format: "LRC"}
	a.redraw()
}

func (a *app) lyricDownloadDialog(c *ui.Context) {
	s := &a.lyricDownload
	a.dialog(c, "download-lyrics", 0.25, &s.open, func(panel ui.Element) {
		ink := a.inkOn(a.nowPlaying)
		panel.Width(420).TextColor(ink.text).Label("下载歌词")
		a.pane(panel, 26, a.nowPlaying)
		panel.Children(func() {
			dialogHeader(c, ink, "下载歌词", s.song.Name, func() { s.open = false })
			ui.Column(c).PaddingX(28).Gap(16).Children(func() {
				ui.Row(c).Justify(ui.SpaceBetween).AlignItems(ui.Center).Children(func() {
					ui.Text(c, "包含时间戳").FontSize(14)
					ui.Switch(c.Key("timestamps"), &s.timestamps).Label("切换歌词时间戳")
				})
				ui.Row(c).Justify(ui.SpaceBetween).AlignItems(ui.Center).Children(func() {
					ui.Text(c, "包含翻译").FontSize(14)
					ui.Switch(c.Key("translations"), &s.translations).Label("切换歌词翻译")
				})
				ui.Row(c).Justify(ui.SpaceBetween).AlignItems(ui.Center).Disabled(!s.timestamps).Children(func() {
					ui.Text(c, "时间戳格式").FontSize(14)
					a.paneSelect(c.Key("format"), "歌词文件格式", &s.format, []string{"LRC", "SRT"})
				})
				hint := "LRC 保留歌词时间轴，适合音乐播放器。"
				if !s.timestamps {
					hint = "保存为 TXT 纯文本，不包含时间戳。"
				} else if s.format == "SRT" {
					hint = "SRT 包含字幕起止时间，适合导入 PR、达芬奇等剪辑软件。"
				}
				ui.Text(c, hint).FontSize(12.5).LineHeight(1.5).TextColor(ink.muted)
			})
			dialogFooter(c, func() {
				if settingButton(c.Key("cancel"), ink, "取消", nil, false, false).Clicked() {
					s.open = false
				}
				if primaryButton(c.Key("save"), "选择保存位置", iconDownload, a.pal, false).Disabled(s.saving).Clicked() && !s.saving {
					format := "txt"
					if s.timestamps {
						format = strings.ToLower(s.format)
					}
					r := a.lyricDownloadRequest(s.song, format, s.translations)
					s.open, s.saving = false, true
					go func() {
						path, err := r.save()
						a.post(func() {
							s.saving = false
							if err != nil {
								a.toast = "下载歌词失败：" + err.Error()
							} else if path != "" {
								a.toast = "歌词已保存：" + path
							}
						})
					}()
				}
			})
		})
	})
}

// A request keeps the song, options and cached lyrics from the UI thread.
// Changing tracks while a file dialog or network request is open cannot change it.
type lyricDownloadRequest struct {
	song         netease.Track
	lines        []lyrics.Line
	loaded       bool
	format       string
	translations bool
	folder       string
	parent       *mygo.Window
}

func (a *app) lyricDownloadRequest(song netease.Track, format string, translations bool) lyricDownloadRequest {
	r := lyricDownloadRequest{song: song, format: format, translations: translations, folder: a.downloadFolder(), parent: a.win}
	if current := a.pl.Current(); current != nil && current.ID == song.ID && current.LocalPath == song.LocalPath {
		lines, loaded := a.pl.Lyrics()
		r.lines, r.loaded = append([]lyrics.Line(nil), lines...), loaded
	}
	return r
}

// Tests replace the network and native save dialog; file writes remain real.
var downloadLyricsOf = netease.Lyrics
var chooseLyricPath = mygo.Dialog.Save

func (r lyricDownloadRequest) load() ([]lyrics.Line, error) {
	if r.loaded {
		return r.lines, nil
	}
	var fetchErr error
	if r.song.ID > 0 {
		lrc, trans, yrc, err := downloadLyricsOf(r.song.ID)
		fetchErr = err
		if err == nil {
			if lines := lyrics.Build(lrc, trans, yrc); len(lines) > 0 {
				return lines, nil
			}
		}
	}
	if r.song.LocalPath != "" {
		b, err := os.ReadFile(download.LyricsPath(r.song.LocalPath))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if lines := lyrics.FromText(string(b)); len(lines) > 0 {
			return lines, nil
		}
		if lines := lyrics.FromText(local.LyricsOf(r.song.LocalPath)); len(lines) > 0 {
			return lines, nil
		}
	}
	return nil, fetchErr
}

func (r lyricDownloadRequest) save() (string, error) {
	lines, err := r.load()
	if err != nil {
		return "", err
	}
	text, err := lyricsFileText(lines, r.format, r.translations, time.Duration(r.song.Duration)*time.Millisecond)
	if err != nil {
		return "", err
	}
	path, err := chooseLyricPath(mygo.SaveDialogOptions{
		Parent: r.parent, Title: "下载歌词", ButtonLabel: "保存",
		DefaultPath:       filepath.Join(r.folder, download.FileName(r.song, r.format)),
		Filters:           []mygo.FileFilter{{Name: strings.ToUpper(r.format) + " 文件", Extensions: []string{r.format}}},
		CreateDirectories: true,
	})
	if err != nil || path == "" {
		return "", err
	}
	ext := "." + r.format
	if filepath.Ext(path) == "" {
		path += ext
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			return "", fmt.Errorf("请在保存窗口中选择 %s 以确认覆盖", filepath.Base(path))
		}
	} else if !strings.EqualFold(filepath.Ext(path), ext) {
		return "", fmt.Errorf("文件扩展名应为 %s", ext)
	}
	if err := writeFileAtomic(path, []byte(text), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func lyricsFileText(lines []lyrics.Line, format string, translations bool, duration time.Duration) (string, error) {
	if format != "txt" && format != "lrc" && format != "srt" {
		return "", errors.New("不支持的歌词格式")
	}
	if format != "txt" && lyrics.IsPlain(lines) {
		return "", errors.New("这首歌没有时间轴，请关闭时间戳后保存为 TXT")
	}
	var out strings.Builder
	cue := 0
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		text := strings.TrimSpace(line.Text)
		if text == "" {
			continue
		}
		trans := ""
		if translations {
			trans = strings.TrimSpace(line.Translation)
		}
		switch format {
		case "txt":
			out.WriteString(text + "\r\n")
			if trans != "" {
				out.WriteString(trans + "\r\n")
			}
		case "lrc":
			ms := max(0, line.At.Milliseconds())
			stamp := fmt.Sprintf("[%02d:%02d.%03d]", ms/60000, ms/1000%60, ms%1000)
			out.WriteString(stamp + text + "\r\n")
			if trans != "" {
				out.WriteString(stamp + trans + "\r\n")
			}
		case "srt":
			start, end := max(0, line.At), line.End
			if trans != "" {
				text += "\r\n" + trans
			}
			// Lines sharing a timestamp form one bilingual or multiline cue.
			for i+1 < len(lines) && lines[i+1].At == line.At {
				i++
				other := lines[i]
				for j, extra := range []string{other.Text, other.Translation} {
					if j == 1 && !translations {
						continue
					}
					if extra = strings.TrimSpace(extra); extra != "" {
						text += "\r\n" + extra
					}
				}
				end = max(end, other.End)
			}
			if end <= start {
				end = start + 5*time.Second
			}
			if i+1 < len(lines) && lines[i+1].At > start {
				end = min(end, lines[i+1].At)
			}
			if duration > start {
				end = min(end, duration)
			}
			if end.Milliseconds() <= start.Milliseconds() {
				end = start + time.Millisecond
			}
			cue++
			fmt.Fprintf(&out, "%d\r\n%s --> %s\r\n%s\r\n\r\n", cue, subtitleTime(start), subtitleTime(end), text)
		}
	}
	if out.Len() == 0 {
		return "", errors.New("这首歌暂无歌词")
	}
	return out.String(), nil
}

func subtitleTime(at time.Duration) string {
	ms := at.Milliseconds()
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}
