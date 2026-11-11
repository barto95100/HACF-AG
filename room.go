package main

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ---------- Salle de vote (adhérents) ----------

type roomData struct {
	page
	Member  Member
	Token   string
	Room    Room
	Error   string
	Done    bool
	Started bool
	Closed  []ScrutinView // résultats des votes clos
}

func (s *Server) roomFor(m Member, token string) roomData {
	rd := roomData{page: s.base(), Member: m, Token: token, Room: s.store.RoomFor(m.ID), Started: s.store.VotingStarted(), Closed: s.store.ClosedScrutins()}
	// tant qu'il n'y a rien à faire, la page se rafraîchit seule ; jamais pendant la saisie d'un vote
	if !rd.Room.Mine && len(rd.Room.Mandants) == 0 {
		rd.Refresh = 10
	}
	return rd
}

func (s *Server) handleRoom(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("t")
	m, ok := s.store.MemberByToken(token)
	if !ok {
		s.notFound(w)
		return
	}
	rd := s.roomFor(m, token)
	rd.Done = r.URL.Query().Get("ok") == "1"
	s.render(w, http.StatusOK, "vote", rd)
}

func (s *Server) handleCast(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "requête invalide", http.StatusBadRequest)
		return
	}
	token := r.PostForm.Get("t")
	m, ok := s.store.MemberByToken(token)
	if !ok {
		s.notFound(w)
		return
	}
	sid, _ := strconv.Atoi(r.PostForm.Get("s"))
	choice, err := strconv.Atoi(r.PostForm.Get("c"))
	if err != nil {
		choice = -1
	}
	mand := map[int]int{}
	for k, v := range r.PostForm {
		if !strings.HasPrefix(k, "m_") || len(v) == 0 || v[0] == "" {
			continue
		}
		owner, e1 := strconv.Atoi(strings.TrimPrefix(k, "m_"))
		c, e2 := strconv.Atoi(v[0])
		if e1 == nil && e2 == nil {
			mand[owner] = c
		}
	}
	if _, err := s.store.Cast(m.ID, sid, choice, mand); err != nil {
		rd := s.roomFor(m, token)
		var re RuleError
		if errors.As(err, &re) {
			rd.Error = re.Error()
			s.render(w, http.StatusUnprocessableEntity, "vote", rd)
			return
		}
		log.Printf("vote: %v", err)
		rd.Error = "Erreur technique, réessayez dans un instant."
		s.render(w, http.StatusInternalServerError, "vote", rd)
		return
	}
	http.Redirect(w, r, "/v?t="+url.QueryEscape(token)+"&ok=1", http.StatusSeeOther)
}

// ---------- Scrutins (administration) ----------

type scrutinsData struct {
	page
	Actor    string
	Message  string
	Scrutins []ScrutinView
	Total    int
}

func (s *Server) handleScrutins(w http.ResponseWriter, r *http.Request, user string) {
	d := scrutinsData{page: s.base(), Actor: user, Message: r.URL.Query().Get("msg"),
		Scrutins: s.store.Scrutins(), Total: s.store.Stats().Total}
	for _, sc := range d.Scrutins {
		if sc.Statut == StatutOuvert {
			d.Refresh = 15 // suivi de la participation en direct
		}
	}
	s.render(w, http.StatusOK, "scrutins", d)
}

func (s *Server) scrutinAction(fn func(id int, user string) error, okMsg string) func(http.ResponseWriter, *http.Request, string) {
	return func(w http.ResponseWriter, r *http.Request, user string) {
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "requête invalide", http.StatusBadRequest)
			return
		}
		id, _ := strconv.Atoi(r.PostForm.Get("id"))
		msg := okMsg
		if err := fn(id, user); err != nil {
			msg = err.Error()
		}
		http.Redirect(w, r, "/admin/scrutins?msg="+url.QueryEscape(msg), http.StatusSeeOther)
	}
}

func (s *Server) handleScrutinCreate(w http.ResponseWriter, r *http.Request, user string) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "requête invalide", http.StatusBadRequest)
		return
	}
	opts := strings.Split(r.PostForm.Get("options"), "\n")
	_, err := s.store.CreateScrutin(r.PostForm.Get("titre"), r.PostForm.Get("description"), r.PostForm.Get("kind"), opts, user)
	msg := "Scrutin créé. Il n'est pas encore ouvert : cliquez sur « Ouvrir le vote » le moment venu."
	if err != nil {
		msg = err.Error()
	}
	http.Redirect(w, r, "/admin/scrutins?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}
