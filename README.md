# ag-hacf

Petite application Go pour l'assemblée générale de HACF : réponses à la convocation
(présent / absent / procuration), votes électroniques anonymes, administration protégée
par forward auth (groupe authentik `Bureau`).

Configuration : voir `.env.example`. Données : `/data` (fichier `state.json`, **données
personnelles : ne jamais le versionner**).

Image : `ghcr.io/barto95100/hacf-ag` (construite par GitHub Actions à chaque push sur `main`).
