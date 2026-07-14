# Memory Watchdog

Surveille les process correspondant à un ou plusieurs motifs et les arrête
(`SIGTERM` puis `SIGKILL`) dès qu'ils dépassent un seuil de mémoire vive (RSS).
L'affichage est un tableau de bord en terminal qui se rafraîchit en place.

> Écrit initialement pour brider Webex (`CiscoCollabHost`), mais utilisable pour
> n'importe quel programme.

## Fonctionnalités

- Surveillance de **plusieurs programmes** simultanément, avec un **seuil de
  mémoire propre** possible par programme.
- Arrêt automatique dès dépassement du seuil : `SIGTERM`, puis `SIGKILL` après
  un délai de grâce si le process résiste.
- Arrêt **manuel** d'un process sélectionné (avec confirmation).
- Tableau de bord temps réel : mémoire par process, jauge d'usage, état.
- Liste triée par usage mémoire décroissant, navigable et défilante.
- **Regroupement par process principal** : les sous-process (onglets d'un
  navigateur, etc.) sont indentés sous leur process parent.
- Barre en bas d'écran : **RAM et CPU système** (utilisé/total, %) et RAM
  cumulée des process surveillés.
- **Notification desktop** quand un process atteint un seuil d'alerte
  (80 % par défaut, configurable).
- Horodatage du **dernier** rafraîchissement et **compte à rebours** du prochain.
- Journal des dernières actions (arrêts déclenchés).
- Configuration par fichier JSON ou arguments de ligne de commande.

## Compilation

```bash
go build -o watchdog ./cmd/watchdog
```

## Utilisation

```bash
# Avec le fichier de config par défaut (watchdog.json dans le dossier courant)
./watchdog

# Avec un fichier de config explicite
./watchdog -config /chemin/vers/ma-config.json

# En surchargeant les motifs directement (prioritaire sur la config)
./watchdog /opt/Webex/bin/CiscoCollabHost /usr/lib/firefox/firefox
```

### Raccourcis clavier

| Touche                 | Action                                        |
| ---------------------- | --------------------------------------------- |
| `q`, `Ctrl+C`, `Esc`   | Quitter                                       |
| `espace`, `r`          | Rafraîchir immédiatement                      |
| `↑`/`↓`, `k`/`j`       | Déplacer la sélection d'une ligne             |
| `PgUp`/`PgDn`, `b`/`f` | Déplacer la sélection d'une page              |
| `g` / `G`              | Aller au début / à la fin de la liste         |
| `x`, `Suppr`           | Tuer le process (ou le groupe) sélectionné (avec confirmation) |

Quand les process à surveiller sont plus nombreux que la hauteur du terminal,
la liste défile : une ligne d'état indique la position (`lignes 3–11 / 221`) et
le nombre de lignes masquées, avec des flèches `↑`/`↓` selon le sens possible.
La ligne sélectionnée est marquée d'un `▸`.

### Arrêt manuel d'un process

Sélectionnez un process avec `↑`/`↓`, puis appuyez sur `x` : une confirmation
apparaît en bas de l'écran. Appuyez sur `y` (ou `o`) pour confirmer l'arrêt,
`n` ou `Échap` pour annuler. L'arrêt suit la même procédure que l'arrêt
automatique : `SIGTERM`, puis `SIGKILL` après le délai de grâce si le process
résiste. La sélection reste sur le même process d'un rafraîchissement à l'autre,
malgré le ré-ordonnancement par usage mémoire.

Si la ligne sélectionnée est un **chef de groupe** (avec des sous-process), la
confirmation propose de tuer **tout le groupe** (chef + sous-process). Le
`SIGTERM` est alors envoyé à tous d'un coup, puis, après un unique délai de
grâce, le `SIGKILL` aux éventuels survivants. Sélectionner un sous-process
individuel ne tue que celui-ci.

## Configuration

Les paramètres se règlent dans un fichier JSON (`watchdog.json` par défaut).
Tous les champs sont **optionnels** : un champ absent conserve sa valeur par
défaut.

```json
{
  "patterns": [
    "/opt/Webex/bin/CiscoCollabHost"
  ],
  "limit": "3GiB",
  "interval": "5s",
  "grace_kill": "2s",
  "bar_width": 20,
  "max_events": 8,
  "notify": true,
  "notify_percent": 80
}
```

| Champ        | Type       | Défaut                              | Description                                                       |
| ------------ | ---------- | ----------------------------------- | ----------------------------------------------------------------- |
| `patterns`   | `[]string` | `["/opt/Webex/bin/CiscoCollabHost"]`| Motifs cherchés dans la ligne de commande (`/proc/<pid>/cmdline`), au seuil par défaut. |
| `targets`    | `[]objet`  | `[]`                                | Motifs avec un **seuil propre** : `{"pattern": "...", "limit": "..."}` (voir ci-dessous). |
| `limit`      | `string`   | `"3GiB"`                            | Seuil de mémoire **par défaut** (motifs de `patterns` et targets sans `limit`). Suffixes : `KiB`/`MiB`/`GiB`, `KB`/`MB`/`GB`, ou octets bruts. |
| `interval`   | `string`   | `"5s"`                              | Fréquence des scans (format Go, ex. `10s`, `1m`).                 |
| `grace_kill` | `string`   | `"2s"`                              | Délai entre `SIGTERM` et `SIGKILL`.                               |
| `bar_width`  | `int`      | `20`                                | Largeur de la jauge d'usage (en caractères).                      |
| `max_events` | `int`      | `8`                                 | Nombre de lignes conservées dans le journal.                      |
| `max_rows`   | `int`      | `0`                                 | Plafond de lignes de process affichées. `0` = auto (s'ajuste à la hauteur du terminal). |
| `notify`     | `bool`     | `true`                              | Activer les notifications desktop d'alerte.                       |
| `notify_percent` | `int`  | `80`                                | Seuil d'alerte, en % du seuil mémoire, déclenchant une notification. |
| `group`      | `bool`     | `true`                              | Regrouper les process sous leur process principal (parent surveillé le plus haut). |

### Seuils par programme

Par défaut, `limit` s'applique à toutes les cibles. Pour donner un seuil propre
à certains programmes, utilisez `targets` : chaque entrée a un `pattern` et,
optionnellement, un `limit` (à défaut, le `limit` global sert de repli).
`patterns` et `targets` se cumulent.

```json
{
  "limit": "3GiB",
  "patterns": ["/opt/Webex/bin/CiscoCollabHost"],
  "targets": [
    { "pattern": "firefox", "limit": "2GiB" },
    { "pattern": "chrome",  "limit": "4GiB" }
  ]
}
```

Ici Webex utilise le seuil par défaut (3 Gio), Firefox 2 Gio et Chrome 4 Gio.
Dans l'en-tête, les cibles au seuil spécifique sont annotées (`firefox (2.00 Go)`)
et le seuil par défaut est libellé `seuil déf.`. Le pourcentage et la jauge de
chaque process sont calculés par rapport à **son** seuil ; le seuil d'alerte
(`notify_percent`) s'applique aussi au seuil propre de chaque programme.

### Priorité des sources

De la plus faible à la plus forte :

1. Valeurs par défaut compilées dans le binaire.
2. Fichier de configuration (`watchdog.json` ou `-config`).
3. Arguments positionnels de ligne de commande (surchargent uniquement `patterns`,
   au seuil par défaut).

## Notifications

Quand un process atteint le seuil d'alerte (`notify_percent`, 80 % par défaut),
une notification desktop est émise via [beeep](https://github.com/gen2brain/beeep)
(D-Bus/`notify-send` sous Linux, natif sous macOS et Windows), et l'alerte est
tracée dans le journal (`⚠ …`). Une notification n'est envoyée qu'une fois par
franchissement : un process repassé sous le seuil pourra à nouveau alerter s'il
le refranchit. Mettre `"notify": false` désactive complètement les notifications.

## Regroupement par process principal

Quand `group` est activé (défaut), chaque process est rattaché à son ancêtre le
plus haut **présent parmi les process surveillés** — son « process principal ».
Les groupes sont triés par mémoire totale décroissante ; au sein d'un groupe, le
chef apparaît en premier (annoté du nombre de process du groupe, ex. `firefox
(27)`), suivi de ses sous-process indentés (`└ …`).

C'est particulièrement utile pour les navigateurs, dont chaque onglet est un
process séparé : ils se regroupent sous le process principal. Notez que si un
motif est très large (ex. `/usr`), l'ancêtre commun peut remonter jusqu'à un
process de session — préférez des motifs applicatifs précis. Mettre
`"group": false` revient à une liste à plat triée par mémoire.

## Fonctionnement

Le programme lit périodiquement `/proc` pour trouver les process dont la ligne
de commande contient l'un des motifs, relève leur `VmRSS`, et déclenche l'arrêt
de ceux qui dépassent le seuil. Il ne se cible jamais lui-même.
