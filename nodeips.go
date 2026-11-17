package main

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// External node IPs set by admins: the host addresses Kubernetes doesn't know (nodes
// registered with an overlay address). Kept in <data>/node-ips.json (mode 600) and shown
// to everyone on the Nodes pages. Matched by host name, ignoring case and the domain, so
// "PBVL-STGCL01-W11" and "pbvl-gencl01-w01.robi.com.bd" both find their node.

type NodeIP struct {
	Host    string    `json:"host"` // as entered
	IP      string    `json:"ip"`
	By      string    `json:"by"`
	Updated time.Time `json:"updated"`
}

type NodeIPStore struct {
	mu   sync.RWMutex
	file string
	m    map[string]NodeIP // nodeKey(host) → entry
}

// nodeKey: lower case, first DNS label ("pbvl-gencl01-w01.robi.com.bd" → "pbvl-gencl01-w01")
func nodeKey(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if i := strings.IndexByte(h, '.'); i > 0 && net.ParseIP(h) == nil {
		h = h[:i]
	}
	return h
}

var hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,252})$`)

func openNodeIPs(dir string) *NodeIPStore {
	s := &NodeIPStore{file: filepath.Join(dir, "node-ips.json"), m: map[string]NodeIP{}}
	if b, err := os.ReadFile(s.file); err == nil {
		var f struct {
			Nodes []NodeIP `json:"nodes"`
		}
		if json.Unmarshal(b, &f) == nil {
			for _, n := range f.Nodes {
				s.m[nodeKey(n.Host)] = n
			}
		} else {
			log.Printf("warning: %s is unreadable; node IPs start empty", s.file)
		}
	}
	return s
}

// Lookup returns the admin-set IP for a node, if any.
func (s *NodeIPStore) Lookup(node string) string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.m[nodeKey(node)].IP
}

func (s *NodeIPStore) list() []NodeIP {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]NodeIP, 0, len(s.m))
	for _, n := range s.m {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return nodeKey(out[i].Host) < nodeKey(out[j].Host) })
	return out
}

func (s *NodeIPStore) saveLocked() error {
	out := make([]NodeIP, 0, len(s.m))
	for _, n := range s.m {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return nodeKey(out[i].Host) < nodeKey(out[j].Host) })
	b, _ := json.MarshalIndent(map[string]any{"nodes": out}, "", "  ")
	if err := os.MkdirAll(filepath.Dir(s.file), 0o700); err != nil {
		return err
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.file)
}

// set stores (or, with ip "", removes) many entries at once.
func (s *NodeIPStore) set(entries map[string]string, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for host, ip := range entries {
		if ip == "" {
			delete(s.m, nodeKey(host))
		} else {
			s.m[nodeKey(host)] = NodeIP{Host: host, IP: ip, By: by, Updated: now}
		}
	}
	return s.saveLocked()
}

func validNodeIP(host, ip string) error {
	if !hostRe.MatchString(host) {
		return fmt.Errorf("%q is not a host name", host)
	}
	if ip != "" && net.ParseIP(ip) == nil {
		return fmt.Errorf("%q is not an IP address", ip)
	}
	return nil
}

// ---------------------------------------------------------------- import

// parseNodeIPRows finds host name + IP pairs in rows (from a paste, CSV or a workbook):
// the first cell that looks like a host name and the first that is an IP. Header and
// other rows are skipped.
func parseNodeIPRows(rows [][]string) (map[string]string, []string) {
	out := map[string]string{}
	var skipped []string
	for _, row := range rows {
		host, ip := "", ""
		for _, c := range row {
			c = strings.TrimSpace(c)
			if c == "" {
				continue
			}
			if ip == "" && net.ParseIP(c) != nil {
				ip = c
			} else if host == "" && net.ParseIP(c) == nil && hostRe.MatchString(c) && strings.ContainsAny(c, "-.0123456789") {
				host = c
			}
		}
		if host != "" && ip != "" {
			out[host] = ip
		} else if line := strings.TrimSpace(strings.Join(row, " ")); line != "" && len(skipped) < 20 {
			skipped = append(skipped, line)
		}
	}
	return out, skipped
}

// textRows splits pasted text or CSV: tabs (copied from Excel), commas, semicolons or spaces.
func textRows(text string) [][]string {
	var rows [][]string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var cells []string
		switch {
		case strings.Contains(line, "\t"):
			cells = strings.Split(line, "\t")
		case strings.Contains(line, ","):
			r := csv.NewReader(strings.NewReader(line))
			r.FieldsPerRecord = -1
			if rec, err := r.Read(); err == nil {
				cells = rec
			} else {
				cells = strings.Split(line, ",")
			}
		case strings.Contains(line, ";"):
			cells = strings.Split(line, ";")
		default:
			cells = strings.Fields(line)
		}
		rows = append(rows, cells)
	}
	return rows
}

// xlsxRows reads every sheet of an .xlsx workbook (cell values only).
func xlsxRows(data []byte) ([][]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("not an .xlsx workbook (protected or old .xls files can't be read: save a copy as .xlsx or CSV)")
	}
	read := func(f *zip.File) ([]byte, error) {
		if f.UncompressedSize64 > 50<<20 {
			return nil, errors.New("workbook too large")
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, 50<<20))
	}
	var shared []string
	var sheets []*zip.File
	for _, f := range zr.File {
		switch {
		case f.Name == "xl/sharedStrings.xml":
			b, err := read(f)
			if err != nil {
				return nil, err
			}
			var sst struct {
				SI []struct {
					T string `xml:"t"`
					R []struct {
						T string `xml:"t"`
					} `xml:"r"`
				} `xml:"si"`
			}
			if err := xml.Unmarshal(b, &sst); err != nil {
				return nil, err
			}
			for _, si := range sst.SI {
				s := si.T
				for _, r := range si.R {
					s += r.T
				}
				shared = append(shared, s)
			}
		case strings.HasPrefix(f.Name, "xl/worksheets/") && strings.HasSuffix(f.Name, ".xml"):
			sheets = append(sheets, f)
		}
	}
	if len(sheets) == 0 {
		return nil, errors.New("no sheets in this workbook")
	}
	sort.Slice(sheets, func(i, j int) bool { return sheets[i].Name < sheets[j].Name })
	var rows [][]string
	for _, f := range sheets {
		b, err := read(f)
		if err != nil {
			return nil, err
		}
		var ws struct {
			Rows []struct {
				C []struct {
					T  string `xml:"t,attr"`
					V  string `xml:"v"`
					IS struct {
						T string `xml:"t"`
					} `xml:"is"`
				} `xml:"c"`
			} `xml:"sheetData>row"`
		}
		if err := xml.Unmarshal(b, &ws); err != nil {
			return nil, err
		}
		for _, r := range ws.Rows {
			var cells []string
			for _, c := range r.C {
				v := c.V
				switch c.T {
				case "s":
					if i, err := strconv.Atoi(v); err == nil && i >= 0 && i < len(shared) {
						v = shared[i]
					}
				case "inlineStr":
					v = c.IS.T
				}
				cells = append(cells, v)
			}
			rows = append(rows, cells)
		}
	}
	return rows, nil
}

// ---------------------------------------------------------------- HTTP

func (s *Server) nodeIPRoutes(mux *http.ServeMux) {
	adminOnly := func(w http.ResponseWriter, r *http.Request) bool {
		if !s.isAdmin(r) {
			writeErr(w, 403, errors.New("only admins can set node IPs"))
			return false
		}
		if s.nodeIPs == nil {
			writeErr(w, 500, errors.New("node IPs are not available"))
			return false
		}
		return true
	}
	who := func(r *http.Request) string {
		if u := reqUser(r); u != nil {
			return u.Username
		}
		return "local"
	}
	mux.HandleFunc("GET /api/nodeips", func(w http.ResponseWriter, r *http.Request) {
		if !adminOnly(w, r) {
			return
		}
		writeJSON(w, 200, s.nodeIPs.list())
	})
	// one node: {host, ip}; an empty ip removes it
	mux.HandleFunc("PUT /api/nodeips", func(w http.ResponseWriter, r *http.Request) {
		if !adminOnly(w, r) {
			return
		}
		var req struct {
			Host string `json:"host"`
			IP   string `json:"ip"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil {
			writeErr(w, 400, errors.New("bad request"))
			return
		}
		req.Host, req.IP = strings.TrimSpace(req.Host), strings.TrimSpace(req.IP)
		if err := validNodeIP(req.Host, req.IP); err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := s.nodeIPs.set(map[string]string{req.Host: req.IP}, who(r)); err != nil {
			writeErr(w, 500, err)
			return
		}
		log.Printf("user %s set node IP %s = %q", who(r), req.Host, req.IP)
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	// many: pasted text / CSV ({"text": …}) or an uploaded .xlsx / .csv file (raw body)
	mux.HandleFunc("POST /api/nodeips/import", func(w http.ResponseWriter, r *http.Request) {
		if !adminOnly(w, r) {
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 10<<20))
		if err != nil {
			writeErr(w, 400, errors.New("file too large (10 MB at most)"))
			return
		}
		var rows [][]string
		switch {
		case bytes.HasPrefix(body, []byte("PK\x03\x04")):
			if rows, err = xlsxRows(body); err != nil {
				writeErr(w, 400, err)
				return
			}
		case bytes.HasPrefix(body, []byte{0xD0, 0xCF, 0x11, 0xE0}):
			writeErr(w, 400, errors.New("this is a protected or old-format Excel file: open it in Excel and save a copy as .xlsx (without a sensitivity label) or CSV"))
			return
		default:
			text := string(body)
			var j struct {
				Text string `json:"text"`
			}
			if strings.HasPrefix(strings.TrimSpace(text), "{") && json.Unmarshal(body, &j) == nil {
				text = j.Text
			}
			rows = textRows(text)
		}
		entries, skipped := parseNodeIPRows(rows)
		if len(entries) == 0 {
			writeErr(w, 400, errors.New("no host name + IP pairs found (one node per line: host name and IP)"))
			return
		}
		if len(entries) > 5000 {
			writeErr(w, 400, errors.New("too many rows (5000 at most)"))
			return
		}
		if err := s.nodeIPs.set(entries, who(r)); err != nil {
			writeErr(w, 500, err)
			return
		}
		log.Printf("user %s imported %d node IPs", who(r), len(entries))
		writeJSON(w, 200, map[string]any{"imported": len(entries), "skipped": skipped})
	})
}
