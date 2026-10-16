package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const schema = "portlease.dev/reservation/v1"

type request struct {
	Names    []string
	Network  string
	Address  string
	Agent    string
	Project  string
	Worktree string
	Service  string
	Label    string
}

type contextInfo struct {
	Agent    string `json:"agent,omitempty"`
	Project  string `json:"project,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Service  string `json:"service,omitempty"`
	Label    string `json:"label,omitempty"`
}

type portInfo struct {
	Name    string `json:"name"`
	Network string `json:"network"`
	Address string `json:"address"`
	Port    int    `json:"port"`
	FD      int    `json:"fd,omitempty"`
	State   string `json:"state"`
}

type result struct {
	Schema           string      `json:"schema"`
	Status           string      `json:"status"`
	Guarantee        string      `json:"guarantee"`
	LeaseID          string      `json:"lease_id"`
	NetworkNamespace string      `json:"network_namespace,omitempty"`
	Context          contextInfo `json:"context,omitempty"`
	Ports            []portInfo  `json:"ports"`
}

type reservation struct {
	Result    result
	listeners []*net.TCPListener
}

func acquire(req request, excluded map[int]bool) (*reservation, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}

	r := &reservation{Result: result{
		Schema:           schema,
		Status:           "ok",
		LeaseID:          randomID(),
		NetworkNamespace: networkNamespace(),
		Context: contextInfo{
			Agent: req.Agent, Project: req.Project, Worktree: req.Worktree,
			Service: req.Service, Label: req.Label,
		},
		Ports: make([]portInfo, 0, len(req.Names)),
	}}

	for _, name := range req.Names {
		var listener *net.TCPListener
		for attempts := 0; attempts < 4096; attempts++ {
			addr := net.JoinHostPort(req.Address, "0")
			ln, err := net.Listen(req.Network, addr)
			if err != nil {
				r.Close()
				return nil, fmt.Errorf("bind %s %s: %w", req.Network, addr, err)
			}
			candidate, ok := ln.(*net.TCPListener)
			if !ok {
				ln.Close()
				r.Close()
				return nil, errors.New("internal error: TCP listener has unexpected type")
			}
			port := candidate.Addr().(*net.TCPAddr).Port
			if excluded[port] {
				candidate.Close()
				continue
			}
			listener = candidate
			break
		}
		if listener == nil {
			r.Close()
			return nil, errors.New("kernel repeatedly selected ports held by active cooperative leases")
		}
		port := listener.Addr().(*net.TCPAddr).Port
		r.listeners = append(r.listeners, listener)
		r.Result.Ports = append(r.Result.Ports, portInfo{
			Name: name, Network: req.Network, Address: req.Address,
			Port: port, State: "held",
		})
	}
	return r, nil
}

func validateRequest(req request) error {
	if len(req.Names) == 0 {
		return errors.New("at least one port is required")
	}
	if len(req.Names) > 128 {
		return errors.New("at most 128 ports may be requested")
	}
	if req.Network != "tcp4" && req.Network != "tcp6" {
		return errors.New("network must be tcp4 or tcp6")
	}
	ip := net.ParseIP(req.Address)
	if ip == nil {
		return errors.New("address must be a literal IP address")
	}
	if req.Network == "tcp4" && ip.To4() == nil {
		return errors.New("tcp4 requires an IPv4 address")
	}
	if req.Network == "tcp6" && (ip.To4() != nil || ip.To16() == nil) {
		return errors.New("tcp6 requires an IPv6 address")
	}
	seen := make(map[string]bool, len(req.Names))
	for _, name := range req.Names {
		if !validName(name) {
			return fmt.Errorf("invalid name %q: use letters, digits, and underscores; start with a letter or underscore", name)
		}
		upper := strings.ToUpper(name)
		if seen[upper] {
			return fmt.Errorf("duplicate name %q", name)
		}
		seen[upper] = true
	}
	return nil
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func (r *reservation) files() ([]*os.File, error) {
	files := make([]*os.File, 0, len(r.listeners))
	for i, listener := range r.listeners {
		file, err := listener.File()
		if err != nil {
			for _, f := range files {
				f.Close()
			}
			return nil, fmt.Errorf("duplicate listener descriptor: %w", err)
		}
		files = append(files, file)
		r.Result.Ports[i].FD = 3 + i
		r.Result.Ports[i].State = "inherited"
	}
	return files, nil
}

func (r *reservation) Close() {
	for _, listener := range r.listeners {
		_ = listener.Close()
	}
	r.listeners = nil
}

func (r *reservation) environment() ([]string, error) {
	metadata, err := json.Marshal(r.Result)
	if err != nil {
		return nil, err
	}
	fdCount := 0
	for _, p := range r.Result.Ports {
		if p.FD > 0 {
			fdCount++
		}
	}
	env := []string{
		"PORTLEASE_ID=" + r.Result.LeaseID,
		"PORTLEASE_JSON=" + string(metadata),
		"PORTLEASE_FDS=" + strconv.Itoa(fdCount),
		"PORTLEASE_NETWORK_NAMESPACE=" + r.Result.NetworkNamespace,
	}
	ctx := r.Result.Context
	for key, value := range map[string]string{
		"PORTLEASE_AGENT": ctx.Agent, "PORTLEASE_PROJECT": ctx.Project,
		"PORTLEASE_WORKTREE": ctx.Worktree, "PORTLEASE_SERVICE": ctx.Service,
		"PORTLEASE_LABEL": ctx.Label,
	} {
		if value != "" {
			env = append(env, key+"="+value)
		}
	}
	for _, p := range r.Result.Ports {
		prefix := "PORTLEASE_" + strings.ToUpper(p.Name)
		env = append(env, prefix+"_PORT="+strconv.Itoa(p.Port))
		if p.FD > 0 {
			env = append(env, prefix+"_FD="+strconv.Itoa(p.FD))
		}
	}
	return env, nil
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(os.Getpid())
}

func networkNamespace() string {
	ns, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return "unknown"
	}
	return ns
}
