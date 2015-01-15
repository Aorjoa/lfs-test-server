package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/gorilla/mux"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
)

const (
	contentMediaType = "application/vnd.git-media"
	metaMediaType    = contentMediaType + "+json"
)

var (
	logger  = log.New(os.Stdout, "harbour:", log.LstdFlags)
	baseUrl string
)

type Meta struct {
	Oid   string           `json:"oid"`
	Size  int64            `json:"size"`
	Links map[string]*link `json:"_links,omitempty"`
}

type apiMeta struct {
	Oid       string `json:"oid"`
	Size      int64  `json:"size"`
	Writeable bool   `json:"writeable"`
	existing  bool   `json:"-"`
}

type link struct {
	Href   string            `json:"href"`
	Header map[string]string `json:"header,omitempty"`
}

func main() {
	var listener net.Listener

	a, err := url.Parse(Config.Address)
	if err != nil {
		log.Fatalf("Could not parse listen address: %s, %s", Config.Address, err)
	}

	switch a.Scheme {
	case "fd":
		fd, err := strconv.Atoi(a.Host)
		if err != nil {
			logger.Fatalf("invalid file descriptor: %s", a.Host)
		}

		f := os.NewFile(uintptr(fd), "harbour")
		listener, err = net.FileListener(f)
		if err != nil {
			logger.Fatalf("Can't listen on fd address: %s, %s", Config.Address, err)
		}
	case "tcp", "tcp4", "tcp6":
		laddr, err := net.ResolveTCPAddr(a.Scheme, a.Host)
		if err != nil {
			logger.Fatalf("Could not resolve listen address: %s, %s", Config.Address, err)
		}

		listener, err = net.ListenTCP(a.Scheme, laddr)
		if err != nil {
			logger.Fatalf("Can't listen on address %s, %s", Config.Address, err)
		}
	default:
		logger.Fatalf("Unsupported listener protocol: %s", a.Scheme)
	}

	tl := NewTrackingListener(listener)

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGHUP)
	go func(c chan os.Signal, listener net.Listener) {
		for {
			sig := <-c
			switch sig {
			case syscall.SIGHUP: // Graceful shutdown
				tl.Close()
			}
		}
	}(c, tl)

	baseUrl = fmt.Sprintf("http://%s", tl.Addr())
	logger.Printf("[%d] Listening on %s (http://%s)", os.Getpid(), Config.Address, baseUrl)
	http.Serve(tl, newServer())
	tl.WaitForChildren()
}

func newServer() http.Handler {
	router := mux.NewRouter()

	o := router.PathPrefix("/{user}/{repo}/objects").Subrouter()
	o.Methods("POST").Headers("Accept", metaMediaType).HandlerFunc(PostHandler)

	s := o.Path("/{oid}").Subrouter()
	s.Methods("GET", "HEAD").Headers("Accept", contentMediaType).HandlerFunc(GetContentHandler)
	s.Methods("GET", "HEAD").Headers("Accept", metaMediaType).HandlerFunc(GetMetaHandler)
	s.Methods("OPTIONS").Headers("Accept", contentMediaType).HandlerFunc(OptionsHandler)
	s.Methods("PUT").Headers("Accept", contentMediaType).HandlerFunc(PutHandler)

	return router
}

func GetContentHandler(w http.ResponseWriter, r *http.Request) {
	meta, err := getMeta(r)
	if err != nil {
		w.WriteHeader(404)
		logRequest(r, 404)
		return
	}

	token := S3SignQuery("GET", oidPath(meta.Oid), 86400)
	w.Header().Set("Location", token.Location)
	w.WriteHeader(302)
	logRequest(r, 302)
}

func GetMetaHandler(w http.ResponseWriter, r *http.Request) {
	m, err := getMeta(r)
	if err != nil {
		w.WriteHeader(404)
		fmt.Fprint(w, `{"message":"Not Found"}`)
		logRequest(r, 404)
		return
	}

	w.Header().Set("Content-Type", metaMediaType)

	meta := newMeta(m, false)
	enc := json.NewEncoder(w)
	enc.Encode(meta)
	logRequest(r, 200)
}

func PostHandler(w http.ResponseWriter, r *http.Request) {
	m, err := sendMeta(r)
	if err != nil {
		w.WriteHeader(404)
		fmt.Fprint(w, `{"message":"Not Found"}`)
		logRequest(r, 404)
		return
	}

	if !m.Writeable {
		w.WriteHeader(403)
		return
	}

	w.Header().Set("Content-Type", metaMediaType)

	if !m.existing {
		w.WriteHeader(201)
	}

	meta := newMeta(m, true)
	enc := json.NewEncoder(w)
	enc.Encode(meta)
	logRequest(r, 201)
}

func OptionsHandler(w http.ResponseWriter, r *http.Request) {
	m, err := getMeta(r)
	if err != nil {
		w.WriteHeader(404)
		logRequest(r, 404)
		return
	}

	if !m.Writeable {
		w.WriteHeader(403)
		logRequest(r, 403)
		return
	}

	if m.Oid == "" {
		w.WriteHeader(204)
		logRequest(r, 204)
	}

	logRequest(r, 200)
}

func PutHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(405)
	logRequest(r, 405)
}

func getMeta(r *http.Request) (*apiMeta, error) {
	vars := mux.Vars(r)
	user := vars["user"]
	repo := vars["repo"]
	oid := vars["oid"]

	authz := r.Header.Get("Authorization")
	url := Config.MetaEndpoint + "/" + filepath.Join(user, repo, oid)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		logger.Printf("[META] error - %s", err)
		return nil, err
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Printf("[META] error - %s", err)
		return nil, err
	}

	defer res.Body.Close()
	if res.StatusCode == 200 {
		var m apiMeta
		dec := json.NewDecoder(res.Body)
		err := dec.Decode(&m)
		if err != nil {
			logger.Printf("[META] error - %s", err)
			return nil, err
		}

		return &m, nil
	}

	logger.Printf("[META] status - %d", res.StatusCode)
	return nil, fmt.Errorf("status: %d", res.StatusCode)
}

func sendMeta(r *http.Request) (*apiMeta, error) {
	vars := mux.Vars(r)
	user := vars["user"]
	repo := vars["repo"]

	var m apiMeta
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(&m)
	if err != nil {
		logger.Printf("[META] error - %s", err)
		return nil, err
	}

	authz := r.Header.Get("Authorization")
	url := Config.MetaEndpoint + "/" + filepath.Join(user, repo, m.Oid)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.Encode(&m)

	req, err := http.NewRequest("POST", url, &buf)
	if err != nil {
		logger.Printf("[META] error - %s", err)
		return nil, err
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Printf("[META] error - %s", err)
		return nil, err
	}

	defer res.Body.Close()
	if res.StatusCode == 200 || res.StatusCode == 201 {
		var m apiMeta
		dec := json.NewDecoder(res.Body)
		err := dec.Decode(&m)
		if err != nil {
			logger.Printf("[META] error - %s", err)
			return nil, err
		}

		m.existing = res.StatusCode == 200

		return &m, nil
	}

	logger.Printf("[META] status - %d", res.StatusCode)
	return nil, fmt.Errorf("status: %d", res.StatusCode)
}

func newMeta(m *apiMeta, upload bool) *Meta {
	meta := &Meta{
		Oid:   m.Oid,
		Size:  m.Size,
		Links: make(map[string]*link),
	}
	meta.Links["download"] = newLink("GET", meta.Oid)
	if upload {
		meta.Links["upload"] = newLink("PUT", meta.Oid)
		meta.Links["callback"] = &link{Href: "http://example.com/callmemaybe"}
	}
	return meta
}

func newLink(method, oid string) *link {
	token := S3SignHeader(method, oidPath(oid), oid)
	header := make(map[string]string)
	header["Authorization"] = token.Token
	header["x-amz-content-sha256"] = oid
	header["x-amz-date"] = token.Time.Format(isoLayout)

	return &link{Href: token.Location, Header: header}
}

func oidPath(oid string) string {
	dir := filepath.Join(oid[0:2], oid[2:4])

	return filepath.Join("/", dir, oid)
}

func logRequest(r *http.Request, status int) {
	logger.Printf("[%s] %s - %d", r.Method, r.URL, status)
}
