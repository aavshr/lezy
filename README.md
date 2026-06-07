# lezy

A live markdown preview server. Watch a directory for markdown files, render them as HTML, and serve them on localhost — changes reload in the browser automatically.

## Install

```sh
go install github.com/aavshr/lezy@latest
```

Or build from source:

```sh
git clone https://github.com/aavshr/lezy
cd lezy
go build -o lezy .
```

## Usage

```sh
lezy [directory] [-port <n>]
```

| Argument | Default | Description |
|---|---|---|
| `directory` | `.` (current dir) | Directory to watch and serve |
| `-port` | `3000` | Preferred port (auto-increments if taken) |

**Examples:**

```sh
lezy                        # serve current directory on :3000
lezy ~/notes                # serve ~/notes on :3000
lezy ~/notes -port 4000     # serve ~/notes on :4000
```

Open the printed URL in your browser. Edit any `.md` file — the page reloads automatically.

## Features

- Recursive directory watching — new subdirectories are picked up automatically
- Directory listings show only `.md` files and subdirectories
- Markdown rendered with [goldmark](https://github.com/yuin/goldmark) (GitHub Flavored Markdown + Typographer)
- Live reload via Server-Sent Events — no WebSocket dependency
- Light and dark mode via `prefers-color-scheme`
- Falls back to the next available port if the preferred one is in use

## Notes

All code in this project was generated using [Claude](https://claude.ai).
