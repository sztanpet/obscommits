package main

import (
	"bytes"
	"html/template"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	urlre = regexp.MustCompile(`https?://[^ ]+\.[^ ]+`)
)

var usedfactoids = map[string]time.Time{}

type Factoid struct {
	Name    string
	Text    string
	Aliases []string
}

type Factoids []*Factoid

func (f Factoids) Len() int           { return len(f) }
func (f Factoids) Less(i, j int) bool { return f[i].Name < f[j].Name }
func (f Factoids) Swap(i, j int)      { f[i], f[j] = f[j], f[i] }

type Factoidtpl struct {
	tpl      *template.Template
	tplmtime time.Time
	tplbuf   *bytes.Buffer
	tpllen   int
	tplout   []byte
	valid    bool
	data     []*Factoid
	sync.Mutex
}

func (f *Factoidtpl) init() {
	f.Lock()
	defer f.Unlock()

	f.tplbuf = bytes.NewBuffer(nil)
	f.tpl = template.New("main").Funcs(template.FuncMap{
		"linkify": func(s string) (ret template.HTML) {
			s = template.HTMLEscapeString(s)
			matches := urlre.FindAllString(s, -1)
			b := bytes.NewBuffer(nil)
			for _, url := range matches {
				b.Reset()
				b.WriteString(`<a target="_blank" href="`)
				b.WriteString(url)
				b.WriteString(`">`)
				b.WriteString(url)
				b.WriteString(`</a>`)
				line, _ := b.ReadString('\x00')
				s = strings.Replace(s, url, line, -1)
			}

			return template.HTML(s)
		},
	})

	go (func() {
		t := time.NewTicker(30 * time.Second)
		for {
			<-t.C
			f.checkTemplateChanged()
		}
	})()

}

func (f *Factoidtpl) invalidate() {
	f.Lock()
	defer f.Unlock()
	f.valid = false
}

func (f *Factoidtpl) execute(w http.ResponseWriter) {
	f.Lock()
	defer f.Unlock()

	w.Write(f.tplout[0:f.tpllen])
}

func (f *Factoidtpl) ensureFreshness() {
	f.Lock()
	defer f.Unlock()
	if f.valid {
		return
	}

	var err error
	f.tpl, err = f.tpl.ParseFiles("factoid.tpl")
	if err != nil {
		F("failed parsing file", err)
	}

	f.ensureData()

	f.tplbuf.Reset()
	f.tpl.ExecuteTemplate(f.tplbuf, "factoid.tpl", f.data)
	f.tpllen = f.tplbuf.Len()
	if cap(f.tplout) < f.tpllen {
		f.tplout = make([]byte, f.tpllen)
	}
	io.ReadFull(f.tplbuf, f.tplout)

	f.valid = true
}

func (f *Factoidtpl) ensureData() {
	statelock.RLock()
	defer statelock.RUnlock()

	aliases := make(map[string][]string)
	for alias, factoid := range state.Factoidaliases {
		aliases[factoid] = append(aliases[factoid], alias)
	}

	for _, a := range aliases {
		sort.Strings(a)
	}

	factoids := make([]*Factoid, 0, len(state.Factoids))
	for name, text := range state.Factoids {
		factoids = append(factoids, &Factoid{
			Name:    name,
			Text:    text,
			Aliases: aliases[name],
		})
	}

	sort.Sort(Factoids(factoids))
	f.data = factoids
}

func (f *Factoidtpl) checkTemplateChanged() {
	f.Lock()
	defer f.Unlock()

	if !f.valid {
		return
	}

	info, err := os.Stat("factoid.tpl")
	if err != nil {
		D("Error stating factoid.tpl", err)
		return
	}

	if !info.ModTime().Equal(f.tplmtime) {
		f.valid = false
	}
}

var factoidtpl = Factoidtpl{}

func initFactoids(hookpath string) {
	factoidtpl.init()

	http.HandleFunc(hookpath, handleFactoidRequest)
}

func handleFactoidRequest(w http.ResponseWriter, r *http.Request) {
	factoidtpl.ensureFreshness()
	factoidtpl.execute(w)
}

func factoidUsedRecently(factoidkey string) bool {
	if lastused, ok := usedfactoids[factoidkey]; ok && time.Since(lastused) < 30*time.Second {
		D("Not handling factoid:", factoidkey, ", because it was used too recently!")
		usedfactoids[factoidkey] = time.Now()
		return true
	}
	usedfactoids[factoidkey] = time.Now()
	return false
}

// checks if there is a factoid, if there isnt tries to look if its an alias
// and then recurses with the found factoid
func getFactoidByKey(factoidkey string) (factoid, key string, ok bool) {
	key = factoidkey
restart:
	if factoid, ok = state.Factoids[key]; ok {
		return
	}
	key, ok = state.Factoidaliases[key]
	if ok {
		goto restart
	}

	return
}

func tryHandleFactoid(target, message string) (abort bool) {
	if len(message) == 0 || message[0:1] != "!" {
		return
	}

	pos := strings.Index(message, " ")
	if pos < 0 {
		pos = len(message)
	}
	factoidkey := strings.ToLower(message[1:pos])
	if !isalpha.MatchString(factoidkey) {
		return
	}
	statelock.Lock()
	defer statelock.Unlock()
	if factoid, factoidkey, ok := getFactoidByKey(factoidkey); ok {
		if factoidUsedRecently(factoidkey) {
			return
		}
		if pos != len(message) { // there was a postfix
			rest := message[pos+1:]        // skip the space
			pos = strings.Index(rest, " ") // and search for the next space
			if pos > 0 {                   // and only print the first thing delimeted by a space
				rest = rest[0:pos]
			}
			srv.privmsg(target, rest, ": ", factoid)
		} else { // otherwise just print the factoid
			srv.privmsg(target, factoid)
		}
	}

	return true
}

func tryHandleAdminFactoid(target, nick string, parts []string) (abort, savestate bool) {

	var newfactoidkey string
	var factoid string
	abort = true
	command := parts[0]
	factoidkey := strings.ToLower(parts[1])

	if len(parts) >= 3 {
		newfactoidkey = strings.ToLower(parts[2])
		factoid = parts[2]
	}

	if len(parts) == 4 {
		factoid = parts[2] + " " + parts[3]
	}

	switch command {
	case "add":
		fallthrough
	case "mod":
		if len(parts) < 3 {
			return
		}
		state.Factoids[factoidkey] = factoid
		savestate = true
		srv.notice(nick, "Added/Modified successfully")

	case "del":
	restartdelete:
		if _, ok := state.Factoids[factoidkey]; ok {
			delete(state.Factoids, factoidkey)
			srv.notice(nick, "Deleted successfully")
			// clean up the aliases too
			for k, v := range state.Factoidaliases {
				if v == factoidkey {
					delete(state.Factoidaliases, k)
				}
			}
		} else if factoidkey, ok = state.Factoidaliases[factoidkey]; ok {
			srv.notice(nick, "Found an alias, deleting the original factoid")
			goto restartdelete
		}

		savestate = true

	case "rename":
		if !isalpha.MatchString(newfactoidkey) {
			return
		}
		if _, ok := state.Factoids[newfactoidkey]; ok {
			srv.notice(nick, "Renaming would overwrite, please delete first")
			return
		}
		if _, ok := state.Factoidaliases[newfactoidkey]; ok {
			srv.notice(nick, "Renaming would overwrite an alias, please delete first")
			return
		}
		if _, ok := state.Factoids[factoidkey]; ok {
			state.Factoids[newfactoidkey] = state.Factoids[factoidkey]
			delete(state.Factoids, factoidkey)
			// rename the aliases too
			for k, v := range state.Factoidaliases {
				if v == factoidkey {
					state.Factoidaliases[k] = newfactoidkey
				}
			}
			savestate = true
			srv.notice(nick, "Renamed successfully")
		} else {
			srv.notice(nick, "Not present")
		}

	case "addalias":
		fallthrough
	case "modalias":
		if len(parts) < 3 {
			return
		}
		// newfactoidkey is the factoid we are going to add an alias for
		// if itself is an alias, get the original factoid key, that is what
		// getFactoidByKey does
		_, newfactoidkey, ok := getFactoidByKey(newfactoidkey)
		if ok {
			state.Factoidaliases[factoidkey] = newfactoidkey
			savestate = true
			srv.notice(nick, "Added/Modified alias for ", newfactoidkey, " successfully")
		} else {
			srv.notice(nick, "No factoid with name ", newfactoidkey, " found")
		}

	case "delalias":
		if _, ok := state.Factoidaliases[factoidkey]; ok {
			srv.notice(nick, "Deleted alias successfully")
			delete(state.Factoidaliases, factoidkey)
			savestate = true
		}

	default:
		abort = false
		return
	}

	if savestate {
		factoidtpl.invalidate()
	}

	return
}
