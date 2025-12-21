# acecinema — Phase I (multi-sites, Caddy statique, 100% Docker)

Objectif: exposer des services publics via Caddy sur le VPS OVH, avec backends répartis sur plusieurs sites reliés en WireGuard. Tout est manuel en Phase I: déploiement des backends via Portainer et configuration statique du Caddyfile.

## Modèle multi-sites (hub & spoke)
- Hub: VPS OVH avec WireGuard + Caddy (reverse_proxy + healthchecks/weights).
- Spokes: sites (Dijon, Thory, …) qui se connectent au hub WireGuard; chaque backend est joignable via son IP WireGuard.
- Convention d’adressage WireGuard (suggestion): un /24 par site, ex:
  - Dijon: 10.200.0.0/24 (backends type 10.200.0.x)
  - Thory: 10.200.1.0/24 (backends type 10.200.1.x)
  - Nouveau site N: 10.200.N.0/24

### Ajouter un site
1) Créer la paire de clés WireGuard du site.
2) Sur le hub (VPS), ajouter le peer avec `AllowedIPs = 10.200.N.0/24`.
3) Sur le site, configurer le peer hub + adresse locale (ex: 10.200.N.2/24).
4) Tester la connectivité: `ping 10.200.0.1` (hub) depuis le site et `ping 10.200.N.2` depuis le hub.

## Déployer un backend (Portainer, manuel)
1) Sur le site cible, placer le compose dans `/opt/docker/acecinema/<service>/docker-compose.yml` (ex: `/opt/docker/acecinema/api/docker-compose.yml`), ou coller le contenu dans Portainer. Template générique: `docs/examples/backend-stack.yml`.
2) Ajuster directement dans le YAML (pas de `.env`):
   - `SITE_NAME` (ex: dijon)
   - `SERVICE_NAME` (ex: api)
   - `PUBLISHED_PORT` (ex: 8080)
   - `TZ`
3) Déployer; vérifier que `/health` répond 200:
   ```
   curl http://10.200.N.X:8080/health
   ```
4) Répéter sur chaque site pour le même service (backends multiples).

## Ajouter le backend dans Caddy (statique)
1) Sur le VPS, éditer le Caddyfile.
2) Ajouter un bloc `reverse_proxy` pour le service, avec `health_uri /health` et des `upstream` par backend avec `lb_weight`.
3) Recharger Caddy (admin API ou `caddy reload`) et vérifier `caddy fmt --validate` avant.

## Exemple concret (service api.acecinema.fr)
- Backends:
  - Dijon: 10.200.0.23:8080 (weight 10)
  - Thory: 10.200.1.45:8080 (weight 1)
- Bloc Caddyfile:
```
api.acecinema.fr {
  encode gzip
  log
  handle {
    reverse_proxy {
      lb_policy random_choose
      lb_try_duration 30s
      lb_try_interval 2s
      health_uri /health
      health_interval 10s
      health_timeout 2s
      upstream 10.200.0.23:8080 {
        lb_weight 10
        max_fails 2
        fail_duration 15s
      }
      upstream 10.200.1.45:8080 {
        lb_weight 1
        max_fails 2
        fail_duration 15s
      }
    }
  }
}
```

## Tester le failover
1) Test de base: `curl -H "Host: api.acecinema.fr" http://<IP_VPS>` et vérifier dans les logs backend du site prioritaire (Dijon).
2) Couper un backend (stop conteneur sur Dijon) → attendre l’intervalle de healthcheck Caddy → la requête doit basculer sur Thory.
3) Couper un site complet (down WireGuard Thory) → Caddy ne doit plus envoyer de trafic vers ses upstreams → vérifier que le service reste servi par les backends restants.
4) Remettre le backend/site → Caddy le réintègre après healthcheck OK.

## Fichiers utiles
- Template stack backend (Portainer): `docs/examples/backend-stack.yml` (copier dans `/opt/docker/acecinema/<service>/docker-compose.yml` si vous déployez en CLI).
- Exemple Caddy pour `api`: `docs/examples/caddy/Caddyfile.api.example`
- Compose Postgres (Phase I, SPOF): `deployments/db-compose.yml` (à placer typiquement dans `/opt/docker/acecinema/db/docker-compose.yml`).

## Hors scope Phase I
- Pas de génération automatique de Caddyfile (édition manuelle uniquement).
- Pas de registry dédiée, pas d’orchestrateur (ni Kubernetes, ni Swarm côté plateforme).
- Pas de déploiement automatique; tout backend est déployé manuellement via Portainer. 
