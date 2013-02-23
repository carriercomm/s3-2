/*
Copyright 2011 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/cgi"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const defaultAddr = ":31798" // default webserver address

var h1TitlePattern = regexp.MustCompile(`<h1>([^<]+)</h1>`)

var (
	httpAddr            = flag.String("http", defaultAddr, "HTTP service address (e.g., '"+defaultAddr+"')")
	httpsAddr           = flag.String("https", "", "HTTPS service address")
	root                = flag.String("root", "", "Website root (parent of 'static', 'content', and 'tmpl")
	gitwebScript        = flag.String("gitwebscript", "/usr/lib/cgi-bin/gitweb.cgi", "Path to gitweb.cgi, or blank to disable.")
	gitwebFiles         = flag.String("gitwebfiles", "/usr/share/gitweb/static", "Path to gitweb's static files.")
	logDir              = flag.String("logdir", "", "Directory to write log files to (one per hour), or empty to not log.")
	logStdout           = flag.Bool("logstdout", true, "Write to stdout?")
	tlsCertFile         = flag.String("tlscert", "", "TLS cert file")
	tlsKeyFile          = flag.String("tlskey", "", "TLS private key file")
	gerritUser          = flag.String("gerrituser", "ubuntu", "Gerrit host's username")
	gerritHost          = flag.String("gerrithost", "", "Gerrit host, or empty.")
	buildbotBackend     = flag.String("buildbot_backend", "", "Build bot status backend URL")
	buildbotHost        = flag.String("buildbot_host", "", "Hostname to map to the buildbot_backend. If an HTTP request with this hostname is received, it proxies to buildbot_backend.")
	pageHtml, errorHtml *template.Template
)

var fmap = template.FuncMap{
	"":        textFmt,
	"html":    htmlFmt,
	"htmlesc": htmlEscFmt,
}

// Template formatter for "" (default) format.
func textFmt(w io.Writer, format string, x ...interface{}) string {
	writeAny(w, false, x[0])
	return ""
}

// Template formatter for "html" format.
func htmlFmt(w io.Writer, format string, x ...interface{}) string {
	writeAny(w, true, x[0])
	return ""
}

// Template formatter for "htmlesc" format.
func htmlEscFmt(w io.Writer, format string, x ...interface{}) string {
	var buf bytes.Buffer
	writeAny(&buf, false, x[0])
	template.HTMLEscape(w, buf.Bytes())
	return ""
}

// Write anything to w; optionally html-escaped.
func writeAny(w io.Writer, html bool, x interface{}) {
	switch v := x.(type) {
	case []byte:
		writeText(w, v, html)
	case string:
		writeText(w, []byte(v), html)
	default:
		if html {
			var buf bytes.Buffer
			fmt.Fprint(&buf, x)
			writeText(w, buf.Bytes(), true)
		} else {
			fmt.Fprint(w, x)
		}
	}
}

// Write text to w; optionally html-escaped.
func writeText(w io.Writer, text []byte, html bool) {
	if html {
		template.HTMLEscape(w, text)
		return
	}
	w.Write(text)
}

func applyTemplate(t *template.Template, name string, data interface{}) []byte {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		log.Printf("%s.Execute: %s", name, err)
	}
	return buf.Bytes()
}

func servePage(w http.ResponseWriter, title, subtitle string, content []byte) {
	d := struct {
		Title    string
		Subtitle string
		Content  template.HTML
	}{
		title,
		subtitle,
		template.HTML(content),
	}

	if err := pageHtml.Execute(w, &d); err != nil {
		log.Printf("godocHTML.Execute: %s", err)
	}
}

func readTemplate(name string) *template.Template {
	fileName := filepath.Join(*root, "tmpl", name)
	data, err := ioutil.ReadFile(fileName)
	if err != nil {
		log.Fatalf("ReadFile %s: %v", fileName, err)
	}
	t, err := template.New(name).Funcs(fmap).Parse(string(data))
	if err != nil {
		log.Fatalf("%s: %v", fileName, err)
	}
	return t
}

func readTemplates() {
	pageHtml = readTemplate("page.html")
	errorHtml = readTemplate("error.html")
}

func serveError(w http.ResponseWriter, r *http.Request, relpath string, err error) {
	contents := applyTemplate(errorHtml, "errorHtml", err) // err may contain an absolute path!
	w.WriteHeader(http.StatusNotFound)
	servePage(w, "File "+relpath, "", contents)
}

func mainHandler(rw http.ResponseWriter, req *http.Request) {
	relPath := req.URL.Path[1:] // serveFile URL paths start with '/'
	if strings.Contains(relPath, "..") {
		return
	}

	if strings.HasPrefix(relPath, "gw/") {
		path := relPath[3:]
		http.Redirect(rw, req, "http://camlistore.org/code/?p=camlistore.git;f="+path+";hb=master", http.StatusFound)
		return
	}

	absPath := filepath.Join(*root, "content", relPath)
	fi, err := os.Lstat(absPath)
	if err != nil {
		log.Print(err)
		serveError(rw, req, relPath, err)
		return
	}
	if fi.IsDir() {
		relPath += "/index.html"
		absPath = filepath.Join(*root, "content", relPath)
		fi, err = os.Lstat(absPath)
		if err != nil {
			log.Print(err)
			serveError(rw, req, relPath, err)
			return
		}
	}

	switch {
	case !fi.IsDir():
		serveFile(rw, req, relPath, absPath)
	}
}

func serveFile(rw http.ResponseWriter, req *http.Request, relPath, absPath string) {
	data, err := ioutil.ReadFile(absPath)
	if err != nil {
		serveError(rw, req, absPath, err)
		return
	}

	title := ""
	if m := h1TitlePattern.FindSubmatch(data); len(m) > 1 {
		title = string(m[1])
	}

	servePage(rw, title, "", data)
}

type gitwebHandler struct {
	Cgi    http.Handler
	Static http.Handler
}

func (h *gitwebHandler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/code/" ||
		strings.HasPrefix(r.URL.Path, "/code/?") {
		h.Cgi.ServeHTTP(rw, r)
	} else {
		h.Static.ServeHTTP(rw, r)
	}
}

func isBot(r *http.Request) bool {
	agent := r.Header.Get("User-Agent")
	return strings.Contains(agent, "Baidu") || strings.Contains(agent, "bingbot") ||
		strings.Contains(agent, "Ezooms") || strings.Contains(agent, "Googlebot")
}

type noWwwHandler struct {
	Handler http.Handler
}

func (h *noWwwHandler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	// Some bots (especially Baidu) don't seem to respect robots.txt and swamp gitweb.cgi,
	// so explicitly protect it from bots.
	if ru := r.URL.RequestURI(); strings.Contains(ru, "/code/") && strings.Contains(ru, "?") && isBot(r) {
		http.Error(rw, "bye", http.StatusUnauthorized)
		log.Printf("bot denied")
		return
	}

	host := strings.ToLower(r.Host)
	if host == "www.camlistore.org" {
		http.Redirect(rw, r, "http://camlistore.org"+r.URL.RequestURI(), http.StatusFound)
		return
	}
	h.Handler.ServeHTTP(rw, r)
}

func fixupGitwebFiles() {
	fi, err := os.Stat(*gitwebFiles)
	if err != nil || !fi.IsDir() {
		if *gitwebFiles == "/usr/share/gitweb/static" {
			// Old Debian/Ubuntu location
			*gitwebFiles = "/usr/share/gitweb"
		}
	}
}

func main() {
	flag.Parse()
	readTemplates()

	if *root == "" {
		var err error
		*root, err = os.Getwd()
		if err != nil {
			log.Fatalf("Failed to getwd: %v", err)
		}
	}

	fixupGitwebFiles()

	latestGits := filepath.Join(*root, "latestgits")
	os.Mkdir(latestGits, 0700)
	if *gerritHost != "" {
		go rsyncFromGerrit(latestGits)
	}

	mux := http.DefaultServeMux
	mux.Handle("/favicon.ico", http.FileServer(http.Dir(filepath.Join(*root, "static"))))
	mux.Handle("/robots.txt", http.FileServer(http.Dir(filepath.Join(*root, "static"))))
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir(filepath.Join(*root, "static")))))
	mux.Handle("/talks/", http.StripPrefix("/talks/", http.FileServer(http.Dir(filepath.Join(*root, "talks")))))
	mux.Handle("/pkg/", godocHandler{})
	mux.Handle("/cmd/", godocHandler{})

	gerritUrl, _ := url.Parse(fmt.Sprintf("http://%s:8000/", *gerritHost))
	var gerritHandler http.Handler = httputil.NewSingleHostReverseProxy(gerritUrl)
	if *httpsAddr != "" {
		proxyHandler := gerritHandler
		gerritHandler = http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			if req.TLS != nil {
				proxyHandler.ServeHTTP(rw, req)
				return
			}
			http.Redirect(rw, req, "https://camlistore.org"+req.URL.RequestURI(), http.StatusFound)
		})
	}
	mux.Handle("/r/", gerritHandler)
	mux.HandleFunc("/debugz/ip", ipHandler)

	testCgi := &cgi.Handler{Path: filepath.Join(*root, "test.cgi"),
		Root: "/test.cgi",
	}
	mux.Handle("/test.cgi", testCgi)
	mux.Handle("/test.cgi/foo", testCgi)
	mux.Handle("/code", http.RedirectHandler("/code/", http.StatusFound))
	if *gitwebScript != "" {
		env := os.Environ()
		env = append(env, "GITWEB_CONFIG="+filepath.Join(*root, "gitweb-camli.conf"))
		env = append(env, "CAMWEB_ROOT="+filepath.Join(*root))
		env = append(env, "CAMWEB_GITDIR="+latestGits)
		mux.Handle("/code/", &fixUpGitwebUrls{&gitwebHandler{
			Cgi: &cgi.Handler{
				Path: *gitwebScript,
				Root: "/code/",
				Env:  env,
			},
			Static: http.StripPrefix("/code/", http.FileServer(http.Dir(*gitwebFiles))),
		}})
	}
	mux.HandleFunc("/issue/", issueRedirect)
	mux.HandleFunc("/", mainHandler)

	if *buildbotHost != "" && *buildbotBackend != "" {
		buildbotUrl, err := url.Parse(*buildbotBackend)
		if err != nil {
			log.Fatalf("Failed to parse %v as a URL: %v", *buildbotBackend, err)
		}
		buildbotHandler := httputil.NewSingleHostReverseProxy(buildbotUrl)
		bbhpattern := strings.TrimRight(*buildbotHost, "/") + "/"
		mux.Handle(bbhpattern, buildbotHandler)
	}

	var handler http.Handler = &noWwwHandler{Handler: mux}
	if *logDir != "" || *logStdout {
		handler = NewLoggingHandler(handler, *logDir, *logStdout)
	}

	errch := make(chan error)

	httpServer := &http.Server{
		Addr:         *httpAddr,
		Handler:      handler,
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 30 * time.Minute,
	}
	go func() {
		errch <- httpServer.ListenAndServe()
	}()

	if *httpsAddr != "" {
		log.Printf("Starting TLS server on %s", *httpsAddr)
		httpsServer := new(http.Server)
		*httpsServer = *httpServer
		httpsServer.Addr = *httpsAddr
		go func() {
			errch <- httpsServer.ListenAndServeTLS(*tlsCertFile, *tlsKeyFile)
		}()
	}

	log.Fatalf("Serve error: %v", <-errch)
}

var issueNum = regexp.MustCompile(`^/issue/(\d+)$`)

func issueRedirect(w http.ResponseWriter, r *http.Request) {
	m := issueNum.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.Error(w, "Bad request", 400)
		return
	}
	http.Redirect(w, r, "https://code.google.com/p/camlistore/issues/detail?id="+m[1], http.StatusFound)
}

type fixUpGitwebUrls struct {
	handler http.Handler
}

// Not sure what's making these broken URLs like:
//
//   http://localhost:8080/code/?p=camlistore.git%3Bf=doc/json-signing/json-signing.txt%3Bhb=master
//
// ... but something is.  Maybe Buzz?  For now just re-write them
// . Doesn't seem to be a bug in the CGI implementation, though, which
// is what I'd originally suspected.
func (fu *fixUpGitwebUrls) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	oldUrl := req.URL.String()
	newUrl := strings.Replace(oldUrl, "%3B", ";", -1)
	if newUrl == oldUrl {
		fu.handler.ServeHTTP(rw, req)
		return
	}
	http.Redirect(rw, req, newUrl, http.StatusFound)
}

func rsyncFromGerrit(dest string) {
	for {
		err := exec.Command("rsync", "-avPW", *gerritUser+"@"+*gerritHost+":gerrit/git/", dest+"/").Run()
		if err != nil {
			log.Printf("rsync from gerrit = %v", err)
		}
		time.Sleep(10e9)
	}
}

func ipHandler(w http.ResponseWriter, r *http.Request) {
	out, _ := exec.Command("ip", "-f", "inet", "addr", "show", "dev", "eth0").Output()
	str := string(out)
	pos := strings.Index(str, "inet ")
	if pos == -1 {
		return
	}
	str = str[pos+5:]
	pos = strings.Index(str, "/")
	if pos == -1 {
		return
	}
	str = str[:pos]
	w.Write([]byte(str))
}
