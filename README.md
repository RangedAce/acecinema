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
1) Sur le site cible, placer le compose dans `/opt/docker/acecinema/<service>/docker-compose.yml` (ex: `/opt/docker/acecinema/api/docker-compose.yml`), ou coller le contenu dans Portainer. Template générique: `docs/examples/backend-stack.yml`. Pas de `.env`: tout est inline dans le YAML.
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
- Compose Phase I (image unique) : `deployments/compose-db.yml` (cluster Postgres primaire + réplicas) et `deployments/compose-app.yml` (3 apps placeholders). Tout est inline, pas de `.env`.

## Hors scope Phase I
- Pas de génération automatique de Caddyfile (édition manuelle uniquement).
- Pas de registry dédiée, pas d’orchestrateur (ni Kubernetes, ni Swarm côté plateforme).
- Pas de déploiement automatique; tout backend est déployé manuellement via Portainer. 

## Image unique (app placeholder + Postgres primaire/réplica)
- Dockerfile unique → image `rangedace/acecinema:latest` (Go placeholder + Postgres 15). L’entrypoint lit `ROLE` : `app`, `db-primary` ou `db-replica`. Une seule base de code et une seule image pour tous les rôles.
- App placeholder (`ROLE=app`) : endpoints `GET /health` (200), `GET /info` (json : NODE_NAME/ROLE/DB_HOST/DB_PORT), `GET /db-check` (SELECT 1 via pgx si les variables DB_* sont renseignées).
- Postgres primaire (`ROLE=db-primary`) : init automatique si `$PGDATA` est vide, création utilisateur applicatif (`DB_USER/DB_PASSWORD`), base `DB_NAME` et user de réplication (`REPL_USER/REPL_PASSWORD`). Config réplication : `wal_level=replica`, slots activés, `wal_keep_size=256MB`, `hot_standby=on`, `pg_hba` ouvert (à restreindre via firewall/WG en prod).
- Postgres réplica (`ROLE=db-replica`) : `pg_basebackup` auto avec slot `REPLICATION_SLOT` (défaut = NODE_NAME) vers `MASTER_HOST:MASTER_PORT`, génère `standby.signal` et démarre Postgres.

## Compose Phase I (même image partout)
- Depuis le checkout `/opt/docker/acecinema` (ou stack Portainer), l’image est tirée depuis Docker Hub : `rangedace/acecinema:latest`. Pas de build local requis.
- Compose DB : `deployments/compose-db.yml` (db1 primaire, db2/db3 réplicas, volumes persistants, ports 5432/5433/5434 exposés). Pull & run : `docker compose -f deployments/compose-db.yml pull && docker compose -f deployments/compose-db.yml up -d`.
- Compose App : `deployments/compose-app.yml` (app1/app2/app3 identiques, healthcheck /health, ports 8081/8082/8083). Pull & run : `docker compose -f deployments/compose-app.yml pull && docker compose -f deployments/compose-app.yml up -d`. Utilise le réseau `acecinema-net` (partagé avec la DB).
- Ajouter une réplica DB : copier un service `dbN`, changer `NODE_NAME`, `REPLICATION_SLOT` (optionnel), `ports` (ex: `5435:5432`), volume (ex: `db4-data:`) et `MASTER_HOST` pointant sur le primaire. Redéployer `docker compose -f deployments/compose-db.yml up -d`.
- Ajouter une instance app : copier un service `appN`, ajuster `NODE_NAME`, `APP_SECRET` (placeholder), `ports` (ex: `8084:8080`) et garder `ROLE=app`. Redéployer `docker compose -f deployments/compose-app.yml up -d`.
- Ordre de lancement : 1) `compose-db` (attendre les healthchecks), 2) `compose-app`. Les stacks peuvent être déployées sur des sites distincts via Portainer en utilisant le même YAML (roles + envs suffisent).

## Caddy statique (LB/failover)
- Exemple app placeholder (ports 8081-8083) :
```
app.acecinema.fr {
  encode gzip
  log
  reverse_proxy {
    lb_policy random_choose
    lb_try_duration 20s
    lb_try_interval 2s
    health_uri /health
    health_interval 10s
    upstream 10.200.0.10:8081
    upstream 10.200.1.10:8081
    upstream 10.200.2.10:8081
  }
}
```
- Exemple DB interne : pointer les apps vers le primaire via un endpoint stable (ex: adresse WireGuard du `db1`). Pour un failover manuel, promouvoir une réplica et mettre à jour `MASTER_HOST` sur les autres réplicas, puis redémarrer leurs conteneurs.
- Toujours valider le Caddyfile : `caddy fmt --validate /etc/caddy/Caddyfile` puis `caddy reload`.
