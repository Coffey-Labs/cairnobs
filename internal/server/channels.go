package server

// Alert channels edited in the UI. Like checks
// and maintenance windows, a config file wins: when obs-beacond.yaml has an
// `alerts.channels` key (even an empty list) the UI shows the channels
// read-only. Otherwise they live in the database.
//
// Channels carry secrets: ntfy topics and tokens, Discord and webhook URLs,
// webhook signing secrets, SMTP passwords, PagerDuty integration keys. In the database the whole section
// is sealed with the state key. The secrets are write-only: the UI is told
// which are set, never what they are, and leaving a secret blank when
// editing keeps the stored one. Export leaves them out; import keeps the
// stored secret of a channel with the same id and type when the file has
// none.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/cairnobs/cairnobs/internal/notify"
)

const sectionChannels = "channels"

// channelTypes are the channel types, and the secrets each one takes:
// required first, then optional.
var channelTypes = map[string]struct{ required, optional []string }{
	"ntfy":      {[]string{"topic"}, []string{"token"}},
	"discord":   {[]string{"webhook"}, nil},
	"webhook":   {[]string{"url"}, []string{"secret"}},
	"email":     {[]string{"password"}, nil},
	"pagerduty": {[]string{"routing_key"}, nil},
	"log":       {nil, nil},
}

func (srv *Server) channelsSource() string {
	switch {
	case srv.cfg.Alerts.Channels != nil:
		return sourceFile
	case srv.db != nil:
		return sourceUI
	}
	return ""
}

// uiChannels holds the channels set in the UI, as entered, secrets and all.
type uiChannels struct {
	mu  sync.Mutex
	raw []uiChannel
}

// uiChannel is one alert channel as the database keeps it and export writes
// it: alerts.channels' settings without the *_file paths, with the secrets
// themselves instead.
type uiChannel struct {
	ID              string   `yaml:"id"`
	Type            string   `yaml:"type"`
	Min             string   `yaml:"min,omitempty"`
	Server          string   `yaml:"server,omitempty"`           // ntfy, pagerduty
	MentionCritical bool     `yaml:"mention_critical,omitempty"` // discord
	SMTP            string   `yaml:"smtp,omitempty"`             // email
	Username        string   `yaml:"username,omitempty"`         // email
	From            string   `yaml:"from,omitempty"`             // email
	To              []string `yaml:"to,omitempty"`               // email

	notify.Secrets `yaml:",inline"`
}

type channelsDoc struct {
	Channels []uiChannel `yaml:"channels"`
}

func (c uiChannel) config() notify.Config {
	return notify.Config{ID: c.ID, Type: c.Type, Min: c.Min, Server: c.Server, MentionCritical: c.MentionCritical,
		SMTP: c.SMTP, Username: c.Username, From: c.From, To: c.To, Secrets: c.Secrets}
}

// secret gets or sets one secret by its name.
func (c *uiChannel) secret(name string) *string {
	switch name {
	case "topic":
		return &c.Topic
	case "token":
		return &c.Token
	case "webhook":
		return &c.Webhook
	case "url":
		return &c.URL
	case "secret":
		return &c.Secret
	case "password":
		return &c.Password
	case "routing_key":
		return &c.RoutingKey
	}
	return nil
}

// secretNames are the secrets a channel of this type takes.
func secretNames(typ string) []string {
	t := channelTypes[typ]
	return append(slices.Clone(t.required), t.optional...)
}

// tidy drops what doesn't belong to the channel's type, so a channel
// switched from ntfy to email doesn't keep a topic nobody can see.
func (c uiChannel) tidy() uiChannel {
	out := uiChannel{ID: c.ID, Type: c.Type, Min: c.Min}
	for _, n := range secretNames(c.Type) {
		*out.secret(n) = strings.TrimSpace(*c.secret(n))
	}
	switch c.Type {
	case "ntfy", "pagerduty":
		out.Server = c.Server
	case "discord":
		out.MentionCritical = c.MentionCritical
	case "email":
		out.SMTP, out.Username, out.From, out.To = c.SMTP, c.Username, c.From, c.To
	}
	if out.Min == "warning" {
		out.Min = "" // the default; keeps exports short
	}
	return out
}

// keepSecrets fills the secrets c leaves blank from old, when old is the
// same channel: same id, same type.
func (c *uiChannel) keepSecrets(old []uiChannel) {
	i := slices.IndexFunc(old, func(o uiChannel) bool { return o.ID == c.ID && o.Type == c.Type })
	if i < 0 {
		return
	}
	for _, n := range secretNames(c.Type) {
		if *c.secret(n) == "" {
			*c.secret(n) = *old[i].secret(n)
		}
	}
}

// checkUIChannels checks a whole set of UI channels and builds them.
func checkUIChannels(raw []uiChannel) ([]notify.Channel, error) {
	cfgs := make([]notify.Config, 0, len(raw))
	for _, c := range raw {
		if !settingID.MatchString(c.ID) {
			return nil, fmt.Errorf("channel id %q: letters, digits, space, dot, dash and underscore, up to 64", c.ID)
		}
		if _, ok := channelTypes[c.Type]; !ok {
			return nil, fmt.Errorf("channel %s: type %q isn't ntfy, discord, webhook, email, pagerduty or log", c.ID, c.Type)
		}
		for _, u := range []struct{ name, v string }{{"webhook", c.Webhook}, {"url", c.URL}, {"server", c.Server}} {
			if u.v == "" {
				continue
			}
			p, err := url.Parse(u.v)
			if err != nil || p.Scheme != "https" && p.Scheme != "http" || p.Host == "" {
				// The value may be the secret: don't repeat it.
				return nil, fmt.Errorf("channel %s: the %s isn't an http(s) URL", c.ID, u.name)
			}
		}
		cfgs = append(cfgs, c.config())
	}
	return buildChannels(cfgs)
}

// loadChannels reads the UI channels at start.
func (srv *Server) loadChannels() error {
	if srv.channelsSource() != sourceUI {
		return nil
	}
	s, ok, err := srv.db.Setting(sectionChannels)
	if err != nil || !ok {
		return err
	}
	plain, err := srv.db.Unseal(s.YAML)
	if err != nil {
		return errors.New("stored alert channels can't be unsealed: is this the state key they were sealed with?")
	}
	var d channelsDoc
	if err := decodeStrict(plain, &d); err != nil {
		return err
	}
	chans, err := checkUIChannels(d.Channels)
	if err != nil {
		return err
	}
	srv.uiChans.raw = d.Channels
	srv.alerts.setChannels(chans)
	return nil
}

// putChannels stores and puts in force a whole new set of UI channels; the
// caller holds srv.uiChans.mu.
func (srv *Server) putChannels(raw []uiChannel, who string) error {
	chans, err := checkUIChannels(raw)
	if err != nil {
		return err
	}
	b, err := compactYAML(channelsDoc{raw})
	if err != nil {
		return err
	}
	if err := srv.saveSealed(sectionChannels, b, who); err != nil {
		log.Printf("settings: can't store the alert channels: %v", err)
		return errStore
	}
	srv.uiChans.raw = raw
	srv.alerts.setChannels(chans)
	return nil
}

// errStore is what the browser hears when the database refuses a write.
var errStore = errors.New("the database didn't take the change; the server log says why")

// channelForm is one channel as the UI edits it. Secrets only ever go in:
// Secrets holds the ones being set or replaced, Clear the optional ones to
// remove, and Set (out) names the ones stored.
type channelForm struct {
	ID              string            `json:"id"`
	Type            string            `json:"type"`
	Min             string            `json:"min"`
	Server          string            `json:"server"`
	MentionCritical bool              `json:"mention_critical"`
	SMTP            string            `json:"smtp"`
	Username        string            `json:"username"`
	From            string            `json:"from"`
	To              []string          `json:"to"`
	Secrets         map[string]string `json:"secrets,omitempty"`
	Clear           []string          `json:"clear,omitempty"`
	Set             []string          `json:"set"`
}

func channelFormOf(c uiChannel) channelForm {
	f := channelForm{ID: c.ID, Type: c.Type, Min: c.Min, Server: c.Server, MentionCritical: c.MentionCritical,
		SMTP: c.SMTP, Username: c.Username, From: c.From, To: c.To, Set: []string{}}
	if f.Min == "" {
		f.Min = "warning"
	}
	if f.To == nil {
		f.To = []string{}
	}
	for _, n := range secretNames(c.Type) {
		if *c.secret(n) != "" {
			f.Set = append(f.Set, n)
		}
	}
	return f
}

func (srv *Server) handleSettingsChannels(w http.ResponseWriter, r *http.Request) {
	src := srv.channelsSource()
	out := []channelForm{}
	if src == sourceUI {
		srv.uiChans.mu.Lock()
		for _, c := range srv.uiChans.raw {
			out = append(out, channelFormOf(c))
		}
		srv.uiChans.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": src, "channels": out})
}

// handleChannelPut adds a channel or replaces the one with that id.
func (srv *Server) handleChannelPut(w http.ResponseWriter, r *http.Request) {
	if srv.readOnly(w, srv.channelsSource(), "alert channels", "obs-beacond.yaml") {
		return
	}
	var f channelForm
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&f); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	c := uiChannel{ID: r.PathValue("id"), Type: f.Type, Min: f.Min, Server: strings.TrimSpace(f.Server), MentionCritical: f.MentionCritical,
		SMTP: strings.TrimSpace(f.SMTP), Username: strings.TrimSpace(f.Username), From: strings.TrimSpace(f.From)}
	for _, t := range f.To {
		if t = strings.TrimSpace(t); t != "" {
			c.To = append(c.To, t)
		}
	}
	names := secretNames(c.Type)
	var replaced []string
	for n, v := range f.Secrets {
		if !slices.Contains(names, n) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("A %s channel has no %s.", c.Type, n)})
			return
		}
		if v = strings.TrimSpace(v); v != "" {
			*c.secret(n) = v
			replaced = append(replaced, n)
		}
	}
	srv.uiChans.mu.Lock()
	defer srv.uiChans.mu.Unlock()
	c.keepSecrets(srv.uiChans.raw)
	for _, n := range f.Clear {
		if slices.Contains(channelTypes[c.Type].optional, n) && !slices.Contains(replaced, n) {
			*c.secret(n) = ""
			replaced = append(replaced, n)
		}
	}
	c = c.tidy()
	raw := slices.Clone(srv.uiChans.raw)
	verb := "added an alert channel"
	if i := slices.IndexFunc(raw, func(x uiChannel) bool { return x.ID == c.ID }); i >= 0 {
		raw[i], verb = c, "changed an alert channel"
	} else {
		raw = append(raw, c)
	}
	if err := srv.putChannels(raw, srv.who(r)); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errStore) {
			code = http.StatusInternalServerError
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	detail := fmt.Sprintf("%s: %s, %s and up", c.ID, c.Type, channelFormOf(c).Min)
	if len(replaced) > 0 && verb == "changed an alert channel" {
		slices.Sort(replaced)
		detail += "; new " + strings.Join(replaced, ", ")
	}
	srv.audit(srv.who(r), verb, detail)
	writeJSON(w, http.StatusOK, channelFormOf(c))
}

func (srv *Server) handleChannelDelete(w http.ResponseWriter, r *http.Request) {
	if srv.readOnly(w, srv.channelsSource(), "alert channels", "obs-beacond.yaml") {
		return
	}
	id := r.PathValue("id")
	srv.uiChans.mu.Lock()
	defer srv.uiChans.mu.Unlock()
	i := slices.IndexFunc(srv.uiChans.raw, func(x uiChannel) bool { return x.ID == id })
	if i < 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "No channel called " + id + "."})
		return
	}
	if err := srv.putChannels(slices.Delete(slices.Clone(srv.uiChans.raw), i, i+1), srv.who(r)); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errStore) {
			code = http.StatusInternalServerError
		}
		writeJSON(w, code, map[string]string{"error": "Not removed: " + err.Error()})
		return
	}
	srv.audit(srv.who(r), "removed an alert channel", id)
	w.WriteHeader(http.StatusNoContent)
}

// exportChannels is the channels as YAML, without their secrets.
func (srv *Server) exportChannels() any {
	var raw []uiChannel
	if srv.channelsSource() == sourceUI {
		srv.uiChans.mu.Lock()
		raw = slices.Clone(srv.uiChans.raw)
		srv.uiChans.mu.Unlock()
	} else {
		// From the config file: everything but the *_file paths, which say
		// where the secrets are on this server.
		for _, c := range srv.cfg.Alerts.Channels {
			raw = append(raw, uiChannel{ID: c.ID, Type: c.Type, Min: c.Min, Server: c.Server, MentionCritical: c.MentionCritical,
				SMTP: c.SMTP, Username: c.Username, From: c.From, To: c.To}.tidy())
		}
	}
	for i := range raw {
		raw[i].Secrets = notify.Secrets{}
	}
	return channelsDoc{raw}
}

// importChannels replaces the UI channels with a YAML document, keeping the
// stored secrets of channels the file leaves them out of.
func (srv *Server) importChannels(b []byte, who string) (int, error) {
	var d channelsDoc
	if err := decodeStrict(b, &d); err != nil {
		if strings.Contains(err.Error(), "_file not found") {
			return 0, errors.New("secrets in files (topic_file and the like) only work in obs-beacond.yaml; here, give the secret itself (topic, token, webhook, url, secret, password) or leave it out to keep the one already set")
		}
		return 0, err
	}
	srv.uiChans.mu.Lock()
	defer srv.uiChans.mu.Unlock()
	raw := make([]uiChannel, 0, len(d.Channels))
	for _, c := range d.Channels {
		c.keepSecrets(srv.uiChans.raw)
		raw = append(raw, c.tidy())
	}
	if err := srv.putChannels(raw, who); err != nil {
		return 0, err
	}
	return len(raw), nil
}
