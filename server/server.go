// Package server contains controller and client logic
package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"

	log "github.com/sirupsen/logrus"

	"github.com/gorilla/websocket"
	"github.com/spf13/viper"
	"github.com/synacor/sibyl/deck"
	"github.com/synacor/sibyl/game"
)

const defaultTemplatesDir = "./templates"
const defaultStaticDir = "./static"

// WsRequestAction is a type for representing a web socket action
type WsRequestAction string

// WsRequestAction constants
const (
	WsRequestActionSelectCard WsRequestAction = "select"
	WsRequestActionReveal                     = "reveal"
	WsRequestActionReset                      = "reset"
	WsRequestActionDeck                       = "deck"
	WsRequestActionTopic                      = "topic"
	WsRequestActionUsername                   = "username"
)

// WsRequest is data that was read from a web socket connection
type WsRequest struct {
	Action WsRequestAction `json:"action"`
	Card   int             `json:"card"`
	Deck   string          `json:"deck"`
	Room   string          `json:"room"`
	Token  string          `json:"token"`
	Value  string          `json:"value"`
}

type safeGames struct {
	games map[string]*game.Game
	mutex *sync.RWMutex
}

// Server is the main object that can be used to return an *http.ServeMux object.
type Server struct {
	templatesDir string
	staticDir    string
	templates    map[string]*template.Template
	debug        bool
	destroyGame  chan *game.Game
	safeGames    *safeGames
}

type templateLoader struct {
	templatesDir string
	baseTemplate *template.Template
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

type indexTemplateValues struct {
	RoomNameMaxLength int
	Error             string
}

type roomTemplateValues struct {
	Token             string
	Decks             []string
	DecksJSON         template.JS
	Room              string
	URL               string
	TopicMaxLength    int
	Username          string
	UsernameMaxLength int
}

func init() {
	viper.SetDefault("templates_dir", defaultTemplatesDir)
	viper.SetDefault("static_dir", defaultStaticDir)
	viper.BindEnv("debug")
}

// New returns a new *Server object
func New() *Server {
	templatesDir := viper.GetString("templates_dir")
	t := newTemplateLoader(templatesDir, "template.html")
	c := &Server{
		safeGames: &safeGames{
			games: make(map[string]*game.Game),
			mutex: &sync.RWMutex{},
		},
		destroyGame: make(chan *game.Game),

		templatesDir: templatesDir,
		staticDir:    viper.GetString("static_dir"),
		debug:        viper.GetBool("debug"),
		templates: map[string]*template.Template{
			"index": t.loadTemplate("index.html"),
			"room":  t.loadTemplate("room.html"),
		},
	}

	return c
}

// ServeMux returns a mux that can be used with the listen and server methods in net/http
func (s *Server) ServeMux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/", s.indexHandler)
	m.HandleFunc("/r/", s.roomHandler)
	m.HandleFunc("/ws", s.wsHandler)
	m.HandleFunc("/create", s.createRoomHandler)
	m.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir(s.staticDir))))
	m.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, s.staticDir+"/favicon.ico")
	})

	return m
}

// createRoomHandler handles requests to POST /create
func (s *Server) createRoomHandler(w http.ResponseWriter, r *http.Request) {
	if strings.ToUpper(r.Method) != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	room := r.PostFormValue("room")
	if !game.RoomNameIsValid(room) {
		http.Redirect(w, r, "/?invalid", http.StatusSeeOther)
		return
	}

	defaultDeck := r.PostFormValue("deck")
	if err := s.createGameIfNotExists(room, defaultDeck); err != nil {
		if err == game.ErrInvalidRoomName {
			http.Redirect(w, r, "/?invalid", http.StatusSeeOther)
			return
		}

		log.WithFields(log.Fields{"room": room}).Errorf("could not create room: %v", err)
		http.Redirect(w, r, "/?error", http.StatusSeeOther)
		return
	}

	roomURL := "/r/" + room
	http.Redirect(w, r, roomURL, http.StatusSeeOther)
	return
}

// indexHandler handles requests to /
func (s *Server) indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	values := indexTemplateValues{
		RoomNameMaxLength: game.RoomNameMaxLength,
	}

	r.ParseForm()
	if _, hasInvalid := r.Form["invalid"]; hasInvalid {
		values.Error = fmt.Sprintf("Invalid room name. %s", game.RoomNameValidDescription)
	} else if room := r.FormValue("notfound"); room != "" {
		values.Error = fmt.Sprintf("A room with the name \"%s\" was not found. Create it using the form below!", room)
	} else if _, hasError := r.Form["error"]; hasError {
		values.Error = fmt.Sprintf("We could not complete your request at this time.")
	}

	s.templates["index"].ExecuteTemplate(w, "template.html", &values)
}

// wsHandler handles requests to /ws
func (s *Server) wsHandler(w http.ResponseWriter, r *http.Request) {
	room := r.FormValue("room")
	token := r.FormValue("token")

	g := s.getGameByRoom(room)
	if g == nil {
		log.WithFields(log.Fields{"room": room, "client": r.RemoteAddr}).Warn("could not get game for room")
		return
	}

	if token != g.Token {
		log.WithFields(log.Fields{"room": room, "client": r.RemoteAddr}).Warn("token does not match for room")
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Errorf("could not upgrade connection: %v", err)
		return
	}

	client := NewClient(g, conn, g.NextClientID())
	g.RegisterClient(client)
	defer func() {
		g.UnregisterClient(client)
	}()

	go client.WritePump(s)
	client.ReadPump(s)
}

// roomHandler handles requests to /r/
func (s *Server) roomHandler(w http.ResponseWriter, r *http.Request) {
	// Path looks like /r/foobar, so we want to strip off "/r/" (first 3 chars)
	room := string(r.URL.Path[3:])

	var token string
	g := s.getGameByRoom(room)
	if g == nil {
		http.Redirect(w, r, "/?notfound="+url.QueryEscape(room), http.StatusSeeOther)
		return
	}

	token = g.Token

	deckJSON, _ := json.Marshal(deck.AllDecks)

	decks := make([]string, 0, len(deck.AllDecks))
	for d := range deck.AllDecks {
		decks = append(decks, d)
	}
	sort.Strings(decks)

	values := roomTemplateValues{
		Token:             token,
		Room:              g.Room,
		URL:               r.URL.String(),
		Decks:             decks,
		DecksJSON:         template.JS(string(deckJSON)),
		TopicMaxLength:    game.TopicMaxLength,
		UsernameMaxLength: UsernameMaxLength,
	}
	s.templates["room"].ExecuteTemplate(w, "template.html", &values)
}

func (s *Server) getGameByRoom(room string) *game.Game {
	s.safeGames.mutex.RLock()
	defer s.safeGames.mutex.RUnlock()

	if g, found := s.safeGames.games[s.roomKey(room)]; found {
		return g
	}

	return nil
}

func (s *Server) createGameIfNotExists(room, defaultDeck string) error {
	if s.getGameByRoom(room) != nil {
		return nil
	}

	g, err := game.New(room, defaultDeck, s.destroyGame)
	if err != nil {
		return err
	}

	log.WithFields(log.Fields{"room": g.Room, "token": g.Token}).Info("room created")
	s.safeGames.mutex.Lock()
	s.safeGames.games[s.roomKey(room)] = g
	s.safeGames.mutex.Unlock()

	return nil
}

func (s *Server) roomKey(room string) string {
	return strings.ToLower(room)
}

// HandleWsRequest handles requests that came in from a web socket connection via Client
func (s *Server) HandleWsRequest(c *Client, r *WsRequest) {
	if s.debug {
		b, err := json.Marshal(r)
		if err != nil {
			log.Errorf("could not marshal JSON: %v", err)
		} else {
			log.WithFields(log.Fields{"client": c.Conn.RemoteAddr().String()}).Debugf("received message: %s", string(b))
		}
	}

	if c.Game.Room != r.Room || c.Game.Token != r.Token {
		log.WithFields(log.Fields{"client": c.Conn.RemoteAddr().String()}).Warnf("token is stale. expected (%s, %s), got (%s, %s)", c.Game.Room, c.Game.Token, r.Room, r.Token)
		return
	}

	switch r.Action {
	case WsRequestActionSelectCard:
		c.Game.AddCard(c, r.Card, r.Deck)
	case WsRequestActionReveal:
		c.Game.Reveal()
	case WsRequestActionReset:
		c.Game.Reset()
	case WsRequestActionDeck:
		d, found := deck.AllDecks[r.Deck]
		if found {
			c.Game.SetDeck(d)
		}
	case WsRequestActionTopic:
		c.Game.SetTopic(r.Value)
	case WsRequestActionUsername:
		c.SetName(r.Value)
		c.Game.SendUpdate()
	default:
		log.Errorf("unknown action received via ws: %s", r.Action)
	}
}

// ListenForEvents will listen for various events like when to destroy a game, and when to disconnect the server.
func (s *Server) ListenForEvents(done chan bool) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)

	for {
		select {
		case game := <-s.destroyGame:
			roomKey := s.roomKey(game.Room)
			s.safeGames.mutex.Lock()
			if _, ok := s.safeGames.games[roomKey]; ok {
				delete(s.safeGames.games, roomKey)
				log.WithFields(log.Fields{"room": game.Room, "token": game.Token}).Info("room destroyed")
			}
			s.safeGames.mutex.Unlock()
		case theSig := <-sig:
			if theSig == syscall.SIGUSR1 {
				s.safeGames.mutex.RLock()
				if len(s.safeGames.games) == 0 {
					s.safeGames.mutex.RUnlock()
					log.Info("no active rooms")
					continue
				}

				keys := make([]string, 0, len(s.safeGames.games))
				for key := range s.safeGames.games {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				for i, key := range keys {
					log.WithFields(log.Fields{"room": key, "clients": s.safeGames.games[key].RegisteredClientsCount()}).Infof("room #%d", i+1)
				}
				s.safeGames.mutex.RUnlock()
			} else {
				log.Printf("Shut down.")
				done <- true
				return
			}
		}
	}
}

func newTemplateLoader(templatesDir, baseTemplate string) *templateLoader {
	base := template.Must(template.ParseFiles(fmt.Sprintf("%s/%s", templatesDir, baseTemplate)))

	return &templateLoader{templatesDir, base}
}

func (t *templateLoader) loadTemplate(files ...string) *template.Template {
	paths := make([]string, len(files))
	for i, file := range files {
		paths[i] = fmt.Sprintf("%s/%s", t.templatesDir, file)
	}

	return template.Must(template.Must(t.baseTemplate.Clone()).ParseFiles(paths...))
}
