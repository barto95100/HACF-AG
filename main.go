package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustConfig() Config {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		log.Fatal(err)
	}
	max, err := strconv.Atoi(env("MAX_MANDATS", "3"))
	if err != nil || max < 1 {
		log.Fatal("MAX_MANDATS invalide")
	}
	dl := os.Getenv("DEADLINE")
	if dl == "" {
		log.Fatal("DEADLINE obligatoire (ex: 2026-12-19T23:59:00+01:00)")
	}
	deadline, err := time.Parse(time.RFC3339, dl)
	if err != nil {
		log.Fatalf("DEADLINE invalide: %v", err)
	}
	base := os.Getenv("BASE_URL")
	if base == "" {
		log.Fatal("BASE_URL obligatoire (ex: https://ag.hacf.fr)")
	}
	return Config{
		Listen:       env("LISTEN", ":8080"),
		BaseURL:      base,
		DataDir:      env("DATA_DIR", "/data"),
		MaxMandats:   max,
		Deadline:     deadline,
		AGName:       env("AG_NAME", "Assemblée générale HACF"),
		AGDateText:   os.Getenv("AG_DATE_TEXT"),
		ContactEmail: env("CONTACT_EMAIL", "contact@hacf.fr"),
		AdminGroup:   os.Getenv("ADMIN_GROUP"),
		HdrUser:      env("HDR_USER", "X-Authentik-Username"),
		HdrGroups:    env("HDR_GROUPS", "X-Authentik-Groups"),
		DolibarrURL:  strings.TrimSpace(os.Getenv("DOLIBARR_URL")),
		DolibarrKey:  strings.TrimSpace(os.Getenv("DOLIBARR_API_KEY")),
		DolibarrHost: strings.TrimSpace(os.Getenv("DOLIBARR_HOST")),
		Loc:          loc,
	}
}

func main() {
	cfg := mustConfig()
	st, err := OpenStore(filepath.Join(cfg.DataDir, "state.json"), cfg.MaxMandats, cfg.Deadline)
	if err != nil {
		log.Fatal(err)
	}
	srv, err := NewServer(cfg, st)
	if err != nil {
		log.Fatal(err)
	}
	if cfg.AdminGroup == "" {
		log.Println("ATTENTION: ADMIN_GROUP vide, /admin est désactivé")
	}
	hs := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	log.Printf("écoute sur %s, date limite %s, plafond %d", cfg.Listen, cfg.Deadline.In(cfg.Loc).Format(time.RFC1123), cfg.MaxMandats)
	fmt.Println(hs.ListenAndServe())
}
