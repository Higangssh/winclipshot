# winclipshot

Make `Win+Shift+S` paste as a file path in terminals — so Claude Code, SSH sessions, and other CLIs can read your screenshots.

On macOS, terminals like iTerm2 and tools like Claude Code handle clipboard images natively. On native Windows they can't: the terminal's stdin only accepts text. `winclipshot` bridges the gap — it quietly watches the clipboard, and when a screenshot is about to be pasted into a terminal it swaps the image for the file path of a saved PNG.

## How it works

`winclipshot` doesn't change `Win+Shift+S` or any keybinding. It subscribes to Windows' clipboard-change events (`AddClipboardFormatListener`) and reacts only when a new **image** appears. When it does, one of three things happens:

1. **Focused window is a terminal** → the image is saved to a PNG file and the clipboard is replaced with the file's path. `Ctrl+V` pastes the path.
2. **Focused window is something else, but you focus a terminal within 60 s** → same conversion happens at that moment. Typical case: you screenshot a webpage in Chrome, switch to the terminal, paste.
3. **Focused window is something else, you never visit a terminal** → the image is left on the clipboard untouched. Pasting into Slack/Word/Chrome behaves normally.

The pending window (60 s by default, `-pending` flag) only polls while an undecided image is on the clipboard; the moment the clipboard changes or a terminal is focused the polling ends. Text copy/paste is never touched.

When `winclipshot.exe` is not running, nothing is different — `Win+Shift+S` and the clipboard work exactly like stock Windows.

```
       ┌──────────────────────────┐
       │  User: Win + Shift + S   │
       │  selects a region        │
       └────────────┬─────────────┘
                    │
                    ▼
       ┌──────────────────────────┐
       │ Windows puts the image   │
       │ on the clipboard         │
       └────────────┬─────────────┘
                    │
                    ▼
       ┌──────────────────────────┐
       │ winclipshot notified of  │
       │ clipboard change (OS     │
       │ event, not polling)      │
       └────────────┬─────────────┘
                    │
                    ▼
       ┌──────────────────────────┐
       │ Foreground window is a   │
       │ terminal?                │
       └───────┬─────────────┬────┘
            no │             │ yes
               ▼             ▼
  ┌──────────────────┐  ┌────────────────────────┐
  │ Remember as      │  │ Save PNG to %TEMP%\    │
  │ pending (60 s).  │  │  winclipshot\clip-*.png│
  │ Watch for focus  │  │ Replace clipboard with │
  │ to switch to a   │  │ the file path (text)   │
  │ terminal.        │  │                        │
  └───────┬──────────┘  └───────────┬────────────┘
          │                         │
          ▼                         │
  ┌──────────────────┐               │
  │ Terminal focused │               │
  │ within 60 s?     │               │
  └──┬───────────┬───┘               │
  no │           │ yes               │
     ▼           └─────┐             │
  (clipboard           ▼             ▼
   unchanged,    ┌──────────────────────────┐
   paste as      │ Ctrl+V in terminal →     │
   image in      │ path pasted into prompt  │
   Slack etc.)   └──────────────────────────┘
```

## Install

1. Grab the latest `winclipshot-<ver>-windows-amd64.zip` (or `arm64`) from [Releases](https://github.com/Higangssh/winclipshot/releases).
2. Unzip anywhere you like — it's a single `winclipshot.exe`, no installer, no registry changes.
3. Double-click `winclipshot.exe` to run it. Leave the console window open; closing it quits the tool.

**Run at sign-in (optional):** press `Win+R`, enter `shell:startup`, drop a shortcut to `winclipshot.exe` into the folder that opens. Windows will launch it automatically every time you log in.

**From source:**

```sh
go install github.com/Higangssh/winclipshot@latest
```

## Usage

Run `winclipshot.exe` and leave the console window open. Then use `Win+Shift+S` normally — the tool only reacts when a screenshot actually lands on the clipboard. Stop it with `Ctrl+C` or by closing the window.

## Options

| Flag | Default | Description |
| --- | --- | --- |
| `-out <dir>` | `%TEMP%\winclipshot` | Where to save PNG files. |
| `-terminals <csv>` | — | Extra executable names to treat as terminals (appended to defaults). |
| `-pending <dur>` | `60s` | How long to wait for you to focus a terminal after a screenshot taken in another app. Set to `0s` to disable deferred conversion. |
| `-v` | off | Verbose logging (also logs non-terminal windows where the image was left alone). |
| `-bench` | off | Run a micro-benchmark of the hot path and exit. |

### Default terminal list

`WindowsTerminal.exe`, `wt.exe`, `conhost.exe`, `cmd.exe`, `powershell.exe`, `pwsh.exe`, `alacritty.exe`, `wezterm-gui.exe`, `mintty.exe`, `Hyper.exe`, `Tabby.exe`

Need more? Use `-terminals` — e.g. `winclipshot.exe -terminals kitty.exe,foot.exe`.

## Performance

`winclipshot` is event-driven — it blocks on `AddClipboardFormatListener` and wakes only when the clipboard actually changes. No polling except during the pending window (60 s max, 200 ms interval, only when a screenshot is waiting for a terminal-focus decision).

Measured on a Windows 11 laptop via `winclipshot.exe -bench` (200 iterations, 400 KiB payload — realistic screenshot size):

```
save + clipboard.Write latency
   p50          : 1.10 ms
   p95          : 2.52 ms
   p99          : 3.45 ms
   max          : 5.00 ms
working set    : 15.7 MiB
heap alloc     : 2.2 MiB
```

Idle CPU is effectively 0 %: the process is blocked on an OS message queue and only woken by clipboard change events. End-to-end user-perceived latency from finishing a snip to the path being on the clipboard is dominated by the 150 ms focus-settle delay we wait for Snipping Tool's overlay to close, not by the code path itself.

Run `winclipshot.exe -bench` on your own machine to reproduce.

## FAQ

**Does it interfere with normal screenshots?**
No. The clipboard is only rewritten when a terminal is currently focused, or when you bring a terminal to the foreground within the pending window (60 s by default). Screenshots you never carry to a terminal are left alone; pasting into Slack, Word, or an email still works.

**What if I take a screenshot in Chrome, then focus a terminal just to check something, then want to paste the image into Slack?**
The screenshot will have been converted to a path by the time you visit the terminal. Take a new screenshot after switching to Slack, or shorten `-pending` if this happens often.

**Where do the images go?**
`%TEMP%\winclipshot\clip-<timestamp>.png` by default. Override with `-out`.

**Is my old clipboard content preserved?**
When a screenshot is redirected to a terminal, the image is replaced with the path. The previous clipboard contents are not restored — the design assumes you just took the screenshot for the terminal.

**Does it collect or send anything?**
No network activity at all. Everything is local.

## License

MIT © Sanghee Son
