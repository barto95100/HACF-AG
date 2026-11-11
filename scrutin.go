package main

import (
	"fmt"
	"strings"
	"time"
)

// Scrutins : votes électroniques anonymes de l'assemblée générale (statuts, art. 11).
//
// Anonymat : un scrutin ne conserve que des TOTAUX par option. L'application note
// seulement quelles voix ont été utilisées (émargement, pour empêcher de voter deux
// fois), jamais le choix associé, et aucune date n'est enregistrée avec un vote.

const (
	KindDecision = "decision" // Pour / Contre / Abstention
	KindChoix    = "choix"    // plusieurs options (candidats…) + vote blanc

	StatutBrouillon = "brouillon"
	StatutOuvert    = "ouvert"
	StatutClos      = "clos"
)

type Scrutin struct {
	ID          int          `json:"id"`
	Titre       string       `json:"titre"`
	Description string       `json:"description,omitempty"`
	Kind        string       `json:"kind"`
	Options     []string     `json:"options"`
	Statut      string       `json:"statut"`
	OpenedAt    time.Time    `json:"opened_at,omitempty"`
	ClosedAt    time.Time    `json:"closed_at,omitempty"`
	Membres     int          `json:"membres,omitempty"` // nombre d'adhérents au moment de l'ouverture
	Voices      map[int]int  `json:"voices,omitempty"`  // voix : adhérent représenté -> adhérent qui la porte
	Used        map[int]bool `json:"used,omitempty"`    // voix déjà utilisées
	Counts      []int        `json:"counts,omitempty"`  // totaux par option
}

// ScrutinView ajoute les chiffres utiles à l'affichage.
type ScrutinView struct {
	Scrutin
	Voix      int // voix pouvant voter
	Exprimees int // voix utilisées
	Seuil     int // 25 % des membres
	Quorum    bool
	Majorite  bool   // Pour > Contre (scrutins Pour/Contre/Abstention)
	Pcts      []int  // part de chaque option parmi les voix exprimées
	Gagnants  []int  // index des options en tête (hors abstention / vote blanc, toujours en dernier)
	Issue     string // phrase de résultat, ex. « Adopté » ou « En tête : Alice »
	IssueTon  string // ok, ko ou neutre (couleur d'affichage)
}

func (s *Store) scrutinIdx(id int) int {
	for i := range s.st.Scrutins {
		if s.st.Scrutins[i].ID == id {
			return i
		}
	}
	return -1
}

func (s *Store) votingStartedLocked() bool {
	for _, sc := range s.st.Scrutins {
		if sc.Statut != StatutBrouillon {
			return true
		}
	}
	return false
}

func cleanText(v string, max int) string {
	v = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 32 {
			return -1
		}
		return r
	}, v)
	v = strings.Join(strings.Fields(v), " ")
	if r := []rune(v); len(r) > max {
		v = string(r[:max])
	}
	return v
}

func (s *Store) CreateScrutin(titre, desc, kind string, options []string, actor string) (Scrutin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	titre = cleanText(titre, 200)
	if titre == "" {
		return Scrutin{}, RuleError("Le titre du scrutin est obligatoire.")
	}
	sc := Scrutin{ID: s.st.NextScrutin, Titre: titre, Description: cleanText(desc, 600), Kind: kind, Statut: StatutBrouillon}
	if sc.ID == 0 {
		sc.ID = 1
	}
	switch kind {
	case KindDecision:
		sc.Options = []string{"Pour", "Contre", "Abstention"}
	case KindChoix:
		seen := map[string]bool{}
		for _, o := range options {
			o = cleanText(o, 100)
			if o == "" || seen[strings.ToLower(o)] {
				continue
			}
			seen[strings.ToLower(o)] = true
			sc.Options = append(sc.Options, o)
		}
		if len(sc.Options) < 1 || len(sc.Options) > 12 {
			return Scrutin{}, RuleError("Indiquez entre 1 et 12 options (une par ligne).")
		}
		sc.Options = append(sc.Options, "Vote blanc")
	default:
		return Scrutin{}, RuleError("Type de scrutin invalide.")
	}
	s.st.NextScrutin = sc.ID + 1
	s.st.Scrutins = append(s.st.Scrutins, sc)
	s.audit(actor, "scrutin_cree", fmt.Sprintf("#%d %s", sc.ID, sc.Titre))
	if err := s.persist(); err != nil {
		s.st.Scrutins = s.st.Scrutins[:len(s.st.Scrutins)-1]
		return Scrutin{}, fmt.Errorf("enregistrement impossible: %w", err)
	}
	return sc, nil
}

// OpenScrutin ouvre un vote et fige la liste des voix (présents + procurations).
func (s *Store) OpenScrutin(id int, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.scrutinIdx(id)
	if i < 0 {
		return RuleError("Scrutin introuvable.")
	}
	sc := &s.st.Scrutins[i]
	if sc.Statut != StatutBrouillon {
		return RuleError("Ce scrutin a déjà été ouvert.")
	}
	for _, o := range s.st.Scrutins {
		if o.Statut == StatutOuvert {
			return RuleError("Un autre scrutin est ouvert : clôturez-le avant d'en ouvrir un nouveau.")
		}
	}
	voices := map[int]int{}
	for mid, r := range s.st.Responses {
		switch r.Choix {
		case ChoixPresent:
			voices[mid] = mid
		case ChoixMandat:
			if p, ok := s.st.Responses[r.MandataireID]; ok && p.Choix == ChoixPresent {
				voices[mid] = r.MandataireID
			}
		}
	}
	if len(voices) == 0 {
		return RuleError("Aucune voix : personne n'a encore indiqué être présent(e).")
	}
	prev := *sc
	sc.Statut, sc.OpenedAt, sc.Membres = StatutOuvert, s.Now().UTC(), len(s.st.Members)
	sc.Voices, sc.Used, sc.Counts = voices, map[int]bool{}, make([]int, len(sc.Options))
	s.audit(actor, "scrutin_ouvert", fmt.Sprintf("#%d %s (%d voix)", sc.ID, sc.Titre, len(voices)))
	if err := s.persist(); err != nil {
		*sc = prev
		s.st.Audit = s.st.Audit[:len(s.st.Audit)-1]
		return fmt.Errorf("enregistrement impossible: %w", err)
	}
	return nil
}

func (s *Store) CloseScrutin(id int, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.scrutinIdx(id)
	if i < 0 {
		return RuleError("Scrutin introuvable.")
	}
	sc := &s.st.Scrutins[i]
	if sc.Statut != StatutOuvert {
		return RuleError("Ce scrutin n'est pas ouvert.")
	}
	sc.Statut, sc.ClosedAt = StatutClos, s.Now().UTC()
	s.audit(actor, "scrutin_clos", fmt.Sprintf("#%d %s (%d voix exprimées)", sc.ID, sc.Titre, len(sc.Used)))
	return s.persist()
}

func (s *Store) DeleteScrutin(id int, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.scrutinIdx(id)
	if i < 0 {
		return RuleError("Scrutin introuvable.")
	}
	if s.st.Scrutins[i].Statut != StatutBrouillon {
		return RuleError("Seul un scrutin non ouvert peut être supprimé.")
	}
	s.audit(actor, "scrutin_supprime", fmt.Sprintf("#%d %s", id, s.st.Scrutins[i].Titre))
	s.st.Scrutins = append(s.st.Scrutins[:i], s.st.Scrutins[i+1:]...)
	return s.persist()
}

func (s *Store) view(sc Scrutin) ScrutinView {
	v := ScrutinView{Scrutin: sc, Voix: len(sc.Voices), Exprimees: len(sc.Used)}
	total := sc.Membres
	if sc.Statut == StatutBrouillon {
		total = len(s.st.Members)
	}
	v.Seuil = (total + 3) / 4
	v.Quorum = v.Exprimees >= v.Seuil && v.Exprimees > 0
	v.Pcts = make([]int, len(sc.Options))
	if v.Exprimees > 0 && len(sc.Counts) == len(sc.Options) {
		for i, c := range sc.Counts {
			v.Pcts[i] = c * 100 / v.Exprimees
		}
	}
	if sc.Kind == KindDecision && len(sc.Counts) == 3 {
		v.Majorite = sc.Counts[0] > sc.Counts[1]
	}
	v.issue()
	return v
}

// issue détermine l'option en tête. Abstention (décision) et vote blanc (choix)
// sont comptés mais ne peuvent jamais l'emporter : ils sont toujours la dernière option.
func (v *ScrutinView) issue() {
	v.IssueTon = "neutre"
	n := len(v.Options)
	if v.Statut != StatutClos || len(v.Counts) != n || n < 2 {
		return
	}
	if v.Exprimees == 0 {
		v.Issue = "Aucun vote exprimé."
		return
	}
	best := 0
	for i := 0; i < n-1; i++ {
		if v.Counts[i] > best {
			best = v.Counts[i]
		}
	}
	if best == 0 {
		v.Issue = "Aucune voix sur une option (abstentions ou votes blancs uniquement)."
		return
	}
	for i := 0; i < n-1; i++ {
		if v.Counts[i] == best {
			v.Gagnants = append(v.Gagnants, i)
		}
	}
	if len(v.Gagnants) > 1 {
		names := make([]string, len(v.Gagnants))
		for k, i := range v.Gagnants {
			names[k] = v.Options[i]
		}
		v.Issue = fmt.Sprintf("Égalité (%d voix chacun) : %s.", best, strings.Join(names, ", "))
		return
	}
	w := v.Gagnants[0]
	if v.Kind == KindDecision {
		if w == 0 {
			v.Issue, v.IssueTon = fmt.Sprintf("Adopté : %d voix pour, %d contre.", v.Counts[0], v.Counts[1]), "ok"
		} else {
			v.Issue, v.IssueTon = fmt.Sprintf("Rejeté : %d voix contre, %d pour.", v.Counts[1], v.Counts[0]), "ko"
		}
		return
	}
	v.Issue, v.IssueTon = fmt.Sprintf("En tête : %s (%d voix).", v.Options[w], best), "ok"
}

func (s *Store) Scrutins() []ScrutinView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ScrutinView, 0, len(s.st.Scrutins))
	for _, sc := range s.st.Scrutins {
		out = append(out, s.view(sc))
	}
	return out
}

// ClosedScrutins renvoie les scrutins clos (résultats publics), le plus récent d'abord.
func (s *Store) ClosedScrutins() []ScrutinView {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ScrutinView
	for i := len(s.st.Scrutins) - 1; i >= 0; i-- {
		if s.st.Scrutins[i].Statut == StatutClos {
			out = append(out, s.view(s.st.Scrutins[i]))
		}
	}
	return out
}

// Room décrit ce qu'un adhérent peut faire dans la salle de vote.
type Room struct {
	Open      *Scrutin // scrutin ouvert (nil si aucun)
	Mine      bool     // l'adhérent a sa propre voix à exprimer
	Mandants  []Member // personnes dont il porte la voix et qui n'a pas encore été utilisée
	Voted     bool     // l'adhérent avait des voix et toutes sont utilisées
	HasVoices bool     // l'adhérent détient des voix pour ce scrutin
}

func (s *Store) RoomFor(memberID int) Room {
	s.mu.Lock()
	defer s.mu.Unlock()
	var r Room
	for i := range s.st.Scrutins {
		if s.st.Scrutins[i].Statut != StatutOuvert {
			continue
		}
		sc := s.st.Scrutins[i]
		r.Open = &sc
		for owner, holder := range sc.Voices {
			if holder != memberID {
				continue
			}
			r.HasVoices = true
			if sc.Used[owner] {
				continue
			}
			if owner == memberID {
				r.Mine = true
			} else if mi := s.memberIdx(owner); mi >= 0 {
				r.Mandants = append(r.Mandants, s.st.Members[mi])
			}
		}
		r.Voted = r.HasVoices && !r.Mine && len(r.Mandants) == 0
		break
	}
	return r
}

// Cast enregistre le(s) vote(s) d'un adhérent : sa voix et celles dont il est mandataire.
// choice s'applique à toutes ses voix, sauf celles redéfinies dans mandChoice (par mandant).
func (s *Store) Cast(memberID, scrutinID, choice int, mandChoice map[int]int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.scrutinIdx(scrutinID)
	if i < 0 || s.st.Scrutins[i].Statut != StatutOuvert {
		return 0, RuleError("Ce vote n'est plus ouvert.")
	}
	sc := &s.st.Scrutins[i]
	valid := func(c int) bool { return c >= 0 && c < len(sc.Options) }
	if !valid(choice) {
		return 0, RuleError("Choix invalide.")
	}
	var owners []int
	for owner, holder := range sc.Voices {
		if holder == memberID && !sc.Used[owner] {
			owners = append(owners, owner)
		}
	}
	if len(owners) == 0 {
		if _, had := func() (int, bool) {
			for _, h := range sc.Voices {
				if h == memberID {
					return h, true
				}
			}
			return 0, false
		}(); had {
			return 0, RuleError("Vous avez déjà voté pour ce scrutin.")
		}
		return 0, RuleError("Vous n'avez pas de voix pour ce scrutin : il faut avoir répondu « présent » avant l'ouverture du vote.")
	}
	prevCounts := append([]int(nil), sc.Counts...)
	for _, owner := range owners {
		c := choice
		if owner != memberID {
			if mc, ok := mandChoice[owner]; ok && valid(mc) {
				c = mc
			}
		}
		sc.Counts[c]++
		if sc.Used == nil {
			sc.Used = map[int]bool{}
		}
		sc.Used[owner] = true
	}
	// émargement uniquement : aucun lien entre l'adhérent et son choix n'est conservé
	s.audit("adhérent", "a_vote", fmt.Sprintf("%s a voté au scrutin #%d (%d voix)", s.st.Members[s.memberIdx(memberID)].Email, sc.ID, len(owners)))
	if err := s.persist(); err != nil {
		sc.Counts = prevCounts
		for _, o := range owners {
			delete(sc.Used, o)
		}
		s.st.Audit = s.st.Audit[:len(s.st.Audit)-1]
		return 0, fmt.Errorf("enregistrement impossible: %w", err)
	}
	return len(owners), nil
}

// VotingStarted indique qu'au moins un scrutin a été ouvert : l'assemblée a commencé
// et les réponses présent / absent / procuration sont figées.
func (s *Store) VotingStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.votingStartedLocked()
}
