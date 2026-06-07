package main

import (
	"bytes"
	"flag"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

var (
	root string
	hub  = &clientHub{clients: make(map[chan string]struct{})}
	md   = goldmark.New(
		goldmark.WithExtensions(extension.GFM, extension.Typographer),
		goldmark.WithRendererOptions(gmhtml.WithHardWraps(), gmhtml.WithUnsafe()),
	)
)

type clientHub struct {
	mu      sync.Mutex
	clients map[chan string]struct{}
}

func (h *clientHub) add(ch chan string) {
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
}

func (h *clientHub) remove(ch chan string) {
	h.mu.Lock()
	delete(h.clients, ch)
	close(ch)
	h.mu.Unlock()
}

func (h *clientHub) broadcast() {
	h.mu.Lock()
	for ch := range h.clients {
		select {
		case ch <- "reload":
		default:
		}
	}
	h.mu.Unlock()
}

func main() {
	port := flag.Int("port", 3000, "port to listen on")
	flag.Parse()

	dir := "."
	if flag.NArg() > 0 {
		dir = flag.Arg(0)
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		log.Fatal(err)
	}
	root = abs

	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		log.Fatalf("%s is not a directory", root)
	}

	go watch()

	http.HandleFunc("/events", sseHandler)
	http.HandleFunc("/", serveHandler)

	ln, actualPort := pickPort(*port)
	if actualPort != *port {
		log.Printf("lezy: port %d in use, using %d instead", *port, actualPort)
	}
	log.Printf("lezy: serving %s at http://localhost:%d", root, actualPort)
	log.Fatal(http.Serve(ln, nil))
}

func pickPort(preferred int) (net.Listener, int) {
	for port := preferred; port < preferred+20; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err == nil {
			return ln, port
		}
	}
	log.Fatalf("lezy: no available port in range %d–%d", preferred, preferred+19)
	return nil, 0
}

func watch() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()

	addDirs := func(path string) {
		filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				watcher.Add(p)
			}
			return nil
		})
	}
	addDirs(root)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			isCreate := event.Has(fsnotify.Create)
			isModify := event.Has(fsnotify.Write) || event.Has(fsnotify.Rename)
			isRemove := event.Has(fsnotify.Remove)

			if isCreate {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					addDirs(event.Name)
				}
			}
			if strings.HasSuffix(event.Name, ".md") && (isCreate || isModify || isRemove) {
				hub.broadcast()
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Println("watcher:", err)
		}
	}
}

func sseHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan string, 4)
	hub.add(ch)
	defer hub.remove(ch)

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func serveHandler(w http.ResponseWriter, r *http.Request) {
	urlPath := r.URL.Path
	fsPath := filepath.Join(root, filepath.FromSlash(urlPath))

	if !strings.HasPrefix(filepath.Clean(fsPath), root) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	info, err := os.Stat(fsPath)
	if err != nil {
		mdPath := fsPath + ".md"
		if mdInfo, err2 := os.Stat(mdPath); err2 == nil && !mdInfo.IsDir() {
			serveMarkdown(w, mdPath)
			return
		}
		http.NotFound(w, r)
		return
	}

	if info.IsDir() {
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		serveDir(w, fsPath, urlPath)
		return
	}

	if strings.HasSuffix(fsPath, ".md") {
		serveMarkdown(w, fsPath)
		return
	}

	http.ServeFile(w, r, fsPath)
}

func serveMarkdown(w http.ResponseWriter, fsPath string) {
	src, err := os.ReadFile(fsPath)
	if err != nil {
		http.Error(w, "could not read file", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := md.Convert(src, &buf); err != nil {
		http.Error(w, "could not convert markdown", http.StatusInternalServerError)
		return
	}

	title := strings.TrimSuffix(filepath.Base(fsPath), ".md")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, renderPage(title, buf.String(), false))
}

func serveDir(w http.ResponseWriter, fsPath, urlPath string) {
	entries, err := os.ReadDir(fsPath)
	if err != nil {
		http.Error(w, "could not read directory", http.StatusInternalServerError)
		return
	}

	type entry struct {
		name  string
		isDir bool
	}
	var items []entry
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			items = append(items, entry{name, true})
		} else if strings.HasSuffix(name, ".md") {
			items = append(items, entry{name, false})
		}
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].isDir != items[j].isDir {
			return items[i].isDir
		}
		return items[i].name < items[j].name
	})

	var sb strings.Builder
	sb.WriteString("<ul class=\"dir-listing\">")

	if urlPath != "/" {
		sb.WriteString(`<li><a href="../" class="dir-entry">../</a></li>`)
	}

	for _, item := range items {
		name := html.EscapeString(item.name)
		href := name
		cssClass := "file-entry"
		if item.isDir {
			href = name + "/"
			cssClass = "dir-entry"
		}
		fmt.Fprintf(&sb, `<li><a href="%s" class="%s">%s</a></li>`, href, cssClass, href)
	}

	sb.WriteString("</ul>")

	title := "Index of " + urlPath
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, renderPage(title, fmt.Sprintf("<h1>%s</h1>\n%s", html.EscapeString(title), sb.String()), true))
}

func renderPage(title, body string, isDir bool) string {
	liveReload := ""
	if !isDir {
		liveReload = `<script>
(function() {
  var es = new EventSource('/events');
  es.onmessage = function(e) { if (e.data === 'reload') location.reload(); };
  es.onerror = function() { setTimeout(function() { location.reload(); }, 1000); };
})();
</script>`
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
<style>
  *, *::before, *::after { box-sizing: border-box; }

  :root {
    --text:       #1f2328;
    --text-muted: #656d76;
    --bg:         #ffffff;
    --bg-subtle:  #f6f8fa;
    --border:     #d0d7de;
    --link:       #0969da;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --text:       #e6edf3;
      --text-muted: #8b949e;
      --bg:         #0d1117;
      --bg-subtle:  #161b22;
      --border:     #30363d;
      --link:       #58a6ff;
    }
  }

  body {
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif;
    font-size: 16px;
    line-height: 1.7;
    color: var(--text);
    background: var(--bg);
    max-width: 800px;
    margin: 0 auto;
    padding: 2.5rem 1.5rem 5rem;
  }

  a { color: var(--link); text-decoration: none; }
  a:hover { text-decoration: underline; }

  p { margin: 0 0 1em; }

  h1, h2, h3, h4, h5, h6 {
    margin: 1.5em 0 0.5em;
    font-weight: 600;
    line-height: 1.25;
  }
  h1 { font-size: 2em;    border-bottom: 1px solid var(--border); padding-bottom: 0.3em; }
  h2 { font-size: 1.5em;  border-bottom: 1px solid var(--border); padding-bottom: 0.2em; }
  h3 { font-size: 1.25em; }
  h4 { font-size: 1em; }
  h5 { font-size: 0.875em; }
  h6 { font-size: 0.85em; color: var(--text-muted); }

  code {
    font-family: "SF Mono", ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    font-size: 0.85em;
    background: var(--bg-subtle);
    border: 1px solid var(--border);
    padding: 0.15em 0.4em;
    border-radius: 6px;
  }
  pre {
    background: var(--bg-subtle);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 1.1em 1.3em;
    overflow-x: auto;
    line-height: 1.5;
    margin: 0 0 1em;
  }
  pre code { background: none; border: none; padding: 0; font-size: 0.875em; }

  blockquote {
    border-left: 4px solid var(--border);
    margin: 1em 0;
    padding: 0.4em 1em;
    color: var(--text-muted);
  }
  blockquote > *:last-child { margin-bottom: 0; }

  table {
    border-collapse: collapse;
    width: 100%%;
    margin: 1em 0;
    display: block;
    overflow-x: auto;
  }
  th, td { border: 1px solid var(--border); padding: 0.4em 0.75em; text-align: left; }
  th { background: var(--bg-subtle); font-weight: 600; }

  ul, ol { padding-left: 1.5em; margin: 0 0 1em; }
  li + li { margin-top: 0.25em; }
  li > ul, li > ol { margin: 0.25em 0 0; }

  img { max-width: 100%%; height: auto; border-radius: 4px; }

  hr { border: none; border-top: 1px solid var(--border); margin: 2em 0; }

  ::selection { background: #b6d4fe; color: #0d1117; }
  @media (prefers-color-scheme: dark) {
    ::selection { background: #388bfd33; color: var(--text); }
  }

  /* Directory listing */
  ul.dir-listing {
    list-style: none;
    padding: 0;
    margin: 0.5em 0;
    font-family: "SF Mono", ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    font-size: 0.9em;
    line-height: 1.4;
  }
  ul.dir-listing li { padding: 0.3em 0; }
  ul.dir-listing .dir-entry { font-weight: 600; }
  ul.dir-listing .dir-entry::before { content: "▸ "; color: var(--text-muted); }
  ul.dir-listing .file-entry::before { content: "  "; white-space: pre; }
</style>
</head>
<body>
%s
%s
</body>
</html>`, html.EscapeString(title), body, liveReload)
}
