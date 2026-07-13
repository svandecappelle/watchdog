// Memory Watchdog — surveille les process correspondant à un motif et les
// arrête (SIGTERM puis SIGKILL) dès qu'ils dépassent un seuil de RAM.
// Affichage sous forme de tableau de bord qui se rafraîchit en place.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gen2brain/beeep"
)

// ─── Configuration ──────────────────────────────────────────────────────────

var (
	// patterns : un ou plusieurs motifs cherchés dans la ligne de commande.
	// Surchargés par les arguments de ligne de commande le cas échéant.
	patterns = []string{"/opt/Webex/bin/CiscoCollabHost"}
	// limitBytes : seuil de mémoire par défaut, appliqué aux motifs sans seuil propre.
	limitBytes = uint64(3) * 1024 * 1024 * 1024 // 3 Gio
	// perPatternLimit : seuil spécifique à certains motifs (surcharge limitBytes).
	perPatternLimit = map[string]uint64{}
	interval        = 5 * time.Second // fréquence des scans
	graceKill       = 2 * time.Second // délai SIGTERM → SIGKILL
	barWidth        = 20              // largeur de la jauge
	maxEvents       = 8               // lignes de journal conservées
	maxRows         = 0               // plafond de lignes visibles (0 = auto, s'ajuste au terminal)

	notifyEnabled = true // notifications desktop activées
	notifyPercent = 80   // seuil de notification, en % du seuil mémoire
)

// notifyFrac renvoie le seuil de notification sous forme de fraction (0.8 pour 80 %).
func notifyFrac() float64 { return float64(notifyPercent) / 100 }

// limitOf renvoie le seuil mémoire applicable à un motif : son seuil propre s'il
// en a un, sinon le seuil par défaut.
func limitOf(pattern string) uint64 {
	if lim, ok := perPatternLimit[pattern]; ok {
		return lim
	}
	return limitBytes
}

// ─── Fichier de configuration ───────────────────────────────────────────────

// target permet de définir un motif avec son propre seuil mémoire.
type target struct {
	Pattern string `json:"pattern"` // motif à surveiller
	Limit   string `json:"limit"`   // seuil propre (optionnel ; sinon "limit" global)
}

// config reflète le fichier JSON. Tous les champs sont optionnels : un champ
// absent ou vide conserve la valeur par défaut ci-dessus.
type config struct {
	Patterns  []string `json:"patterns"`   // motifs au seuil par défaut
	Targets   []target `json:"targets"`    // motifs avec seuil spécifique
	Limit     string   `json:"limit"`      // seuil par défaut, ex. "3GiB", "500MB"
	Interval  string   `json:"interval"`   // durée entre scans, ex. "5s"
	GraceKill string   `json:"grace_kill"` // délai SIGTERM→SIGKILL, ex. "2s"
	BarWidth  int      `json:"bar_width"`  // largeur de la jauge
	MaxEvents int      `json:"max_events"` // lignes de journal conservées
	MaxRows   int      `json:"max_rows"`   // plafond de lignes visibles (0 = auto)

	Notify        *bool `json:"notify"`         // activer les notifications desktop (défaut true)
	NotifyPercent int   `json:"notify_percent"` // seuil de notification en % (défaut 80)
}

// loadConfig lit et applique un fichier de configuration JSON. Renvoie une
// erreur si le fichier est illisible ou mal formé ; l'absence du fichier au
// chemin par défaut n'est pas une erreur (les valeurs par défaut sont gardées).
func loadConfig(path string, mustExist bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !mustExist {
			return nil // pas de fichier : on garde les valeurs par défaut
		}
		return err
	}
	var c config
	if err := json.Unmarshal(data, &c); err != nil {
		return fmt.Errorf("config %s : %w", path, err)
	}

	// Seuil par défaut d'abord : il sert de repli pour les targets sans seuil.
	if c.Limit != "" {
		b, err := parseBytes(c.Limit)
		if err != nil {
			return fmt.Errorf("config %s : champ \"limit\" : %w", path, err)
		}
		limitBytes = b
	}
	// Construit la liste des motifs et leurs seuils propres. Si patterns ou
	// targets sont fournis, on remplace entièrement la cible par défaut.
	if len(c.Patterns) > 0 || len(c.Targets) > 0 {
		patterns = nil
		perPatternLimit = map[string]uint64{}
		patterns = append(patterns, c.Patterns...)
		for i, t := range c.Targets {
			if t.Pattern == "" {
				return fmt.Errorf("config %s : targets[%d] : champ \"pattern\" vide", path, i)
			}
			lim := limitBytes
			if t.Limit != "" {
				b, err := parseBytes(t.Limit)
				if err != nil {
					return fmt.Errorf("config %s : targets[%d] (%s) : champ \"limit\" : %w", path, i, t.Pattern, err)
				}
				lim = b
			}
			patterns = append(patterns, t.Pattern)
			perPatternLimit[t.Pattern] = lim
		}
	}
	if c.Interval != "" {
		d, err := time.ParseDuration(c.Interval)
		if err != nil {
			return fmt.Errorf("config %s : champ \"interval\" : %w", path, err)
		}
		interval = d
	}
	if c.GraceKill != "" {
		d, err := time.ParseDuration(c.GraceKill)
		if err != nil {
			return fmt.Errorf("config %s : champ \"grace_kill\" : %w", path, err)
		}
		graceKill = d
	}
	if c.BarWidth > 0 {
		barWidth = c.BarWidth
	}
	if c.MaxEvents > 0 {
		maxEvents = c.MaxEvents
	}
	if c.MaxRows > 0 {
		maxRows = c.MaxRows
	}
	if c.Notify != nil {
		notifyEnabled = *c.Notify
	}
	if c.NotifyPercent > 0 {
		notifyPercent = c.NotifyPercent
	}
	return nil
}

// parseBytes convertit une taille lisible ("3GiB", "512MB", "1024") en octets.
// Suffixes binaires (KiB/MiB/GiB) et décimaux (KB/MB/GB) acceptés ; sans
// suffixe, la valeur est interprétée en octets.
func parseBytes(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("taille vide")
	}
	units := []struct {
		suffix string
		mult   float64
	}{
		{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
		{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10},
		{"B", 1},
	}
	up := strings.ToUpper(s)
	for _, u := range units {
		if strings.HasSuffix(up, strings.ToUpper(u.suffix)) {
			num := strings.TrimSpace(up[:len(up)-len(u.suffix)])
			v, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, fmt.Errorf("valeur invalide %q", s)
			}
			return uint64(v * u.mult), nil
		}
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("valeur invalide %q", s)
	}
	return v, nil
}

// ─── Accès /proc ────────────────────────────────────────────────────────────

// pidMatch associe un PID au motif qui l'a fait correspondre.
type pidMatch struct {
	pid     int
	pattern string
}

// matchingPIDs scanne /proc une seule fois et renvoie les PID dont la ligne de
// commande contient l'un des motifs. Chaque PID n'apparaît qu'une fois (associé
// au premier motif correspondant).
func matchingPIDs(pats []string) []pidMatch {
	var matches []pidMatch
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return matches
	}
	self := os.Getpid()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue // pas un dossier de PID, ou nous-mêmes
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		// Les arguments sont séparés par des octets nuls.
		cmd := strings.ReplaceAll(string(data), "\x00", " ")
		for _, pat := range pats {
			if strings.Contains(cmd, pat) {
				matches = append(matches, pidMatch{pid: pid, pattern: pat})
				break // un seul motif par PID
			}
		}
	}
	return matches
}

// rssBytes lit VmRSS dans /proc/<pid>/status et le renvoie en octets.
func rssBytes(pid int) (uint64, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line) // "VmRSS:  123456 kB"
			if len(fields) >= 2 {
				if kb, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
					return kb * 1024, true
				}
			}
		}
	}
	return 0, false
}

// cmdName renvoie le premier argument (l'exécutable) d'un process.
func cmdName(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	if parts := strings.Split(string(data), "\x00"); len(parts) > 0 {
		return parts[0]
	}
	return ""
}

// alive teste l'existence d'un process (signal 0).
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// readMem lit /proc/meminfo et renvoie (utilisée, totale) en octets.
func readMem() (used, total uint64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var memTotal, memAvail uint64
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line) // "MemTotal:  32768000 kB"
		if len(f) < 2 {
			continue
		}
		kb, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			memTotal = kb * 1024
		case "MemAvailable:":
			memAvail = kb * 1024
		}
	}
	if memAvail > memTotal {
		memAvail = memTotal
	}
	return memTotal - memAvail, memTotal
}

// readCPU lit la première ligne de /proc/stat et renvoie le cumul de jiffies
// (total, inactif) depuis le démarrage. La différence entre deux relevés donne
// le taux d'occupation CPU.
func readCPU() (total, idle uint64) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	line := data
	if i := strings.IndexByte(string(data), '\n'); i >= 0 {
		line = data[:i]
	}
	f := strings.Fields(string(line)) // "cpu user nice system idle iowait irq softirq steal ..."
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0
	}
	for i := 1; i < len(f); i++ {
		v, err := strconv.ParseUint(f[i], 10, 64)
		if err != nil {
			continue
		}
		total += v
		if i == 4 || i == 5 { // idle + iowait
			idle += v
		}
	}
	return total, idle
}

// ─── Messages & commandes Bubble Tea ────────────────────────────────────────

type procRow struct {
	pid     int
	rss     uint64
	limit   uint64 // seuil applicable à ce process
	name    string
	pattern string
	over    bool
}

type scanMsg struct {
	rows   []procRow
	events []string
}

type tickMsg time.Time

// uiTickMsg déclenche un simple rafraîchissement de l'affichage (compte à
// rebours), indépendamment des scans.
type uiTickMsg time.Time

// uiTick planifie le prochain rafraîchissement d'affichage dans une seconde.
func uiTick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return uiTickMsg(t) })
}

// scan est une tea.Cmd : elle lit l'état des process et tue ceux qui dépassent.
// Elle s'exécute dans une goroutine, donc le SIGTERM→SIGKILL bloquant ne gèle
// pas l'interface.
func scan() tea.Msg {
	var rows []procRow
	var events []string

	for _, mt := range matchingPIDs(patterns) {
		pid := mt.pid
		rss, ok := rssBytes(pid)
		if !ok {
			continue // process disparu entre-temps
		}
		lim := limitOf(mt.pattern)
		row := procRow{pid: pid, rss: rss, limit: lim, name: cmdName(pid), pattern: mt.pattern, over: rss > lim}
		if row.over {
			events = append(events, terminate(pid, fmt.Sprintf("%s > seuil %s", human(rss), human(lim)))...)
		}
		rows = append(rows, row)
	}
	// Tri par défaut : usage mémoire décroissant (donc % décroissant), les
	// process les plus gourmands en tête ; PID croissant à égalité.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].rss != rows[j].rss {
			return rows[i].rss > rows[j].rss
		}
		return rows[i].pid < rows[j].pid
	})
	return scanMsg{rows: rows, events: events}
}

// terminate arrête un process : SIGTERM, puis SIGKILL après le délai de grâce
// s'il résiste. reason décrit le motif de l'arrêt (pour le journal). Renvoie les
// lignes de journal correspondantes. Appel bloquant (à lancer dans une tea.Cmd).
func terminate(pid int, reason string) []string {
	now := func() string { return time.Now().Format("15:04:05") }
	if !alive(pid) {
		return []string{fmt.Sprintf("%s  PID %d déjà arrêté", now(), pid)}
	}
	events := []string{fmt.Sprintf("%s  PID %d (%s) — SIGTERM", now(), pid, reason)}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	time.Sleep(graceKill)
	if alive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		events = append(events, fmt.Sprintf("%s  PID %d récalcitrant — SIGKILL", now(), pid))
	} else {
		events = append(events, fmt.Sprintf("%s  PID %d arrêté proprement", now(), pid))
	}
	return events
}

// notifyCmd envoie une notification desktop par message, dans une goroutine
// (l'appel peut être bloquant selon le backend). Les erreurs sont ignorées :
// une notification ratée ne doit pas perturber la surveillance.
func notifyCmd(messages []string) tea.Cmd {
	return func() tea.Msg {
		for _, msg := range messages {
			_ = beeep.Notify("Memory Watchdog", msg, "")
		}
		return nil
	}
}

// killMsg est renvoyé par killCmd après un arrêt manuel.
type killMsg struct{ events []string }

// killCmd arrête manuellement un process dans une goroutine.
func killCmd(pid int, label string) tea.Cmd {
	return func() tea.Msg {
		return killMsg{events: terminate(pid, "kill manuel — "+label)}
	}
}

// ─── Modèle ─────────────────────────────────────────────────────────────────

type model struct {
	rows        []procRow
	events      []string
	scans       int
	start       time.Time
	lastScan    time.Time    // horodatage du dernier rafraîchissement
	nextScan    time.Time    // horodatage prévu du prochain rafraîchissement
	scroll      int          // index de la première ligne de process affichée
	cursor      int          // index de la ligne sélectionnée
	confirming  bool         // en attente de confirmation d'un kill manuel
	confirmPID  int          // PID visé par la confirmation en cours
	confirmName string       // nom du process visé (affichage)
	notified    map[int]bool // PID déjà notifiés au-dessus du seuil (anti-spam)
	width       int          // dimensions du terminal (0 tant qu'inconnues)
	height      int
	lastErr     string

	// Statistiques système, rafraîchies à chaque tick d'affichage.
	memUsed    uint64  // RAM utilisée (octets)
	memTotal   uint64  // RAM totale (octets)
	cpuPct     float64 // occupation CPU système (%)
	prevCPUTot uint64  // relevé /proc/stat précédent (pour le delta)
	prevCPUIdl uint64
}

func initialModel() model {
	m := model{start: time.Now(), notified: map[int]bool{}}
	m.prevCPUTot, m.prevCPUIdl = readCPU()
	m.memUsed, m.memTotal = readMem()
	return m
}

// refreshSysStats met à jour la RAM et le CPU système (delta depuis le relevé
// précédent). À appeler périodiquement (tick d'affichage).
func (m *model) refreshSysStats() {
	m.memUsed, m.memTotal = readMem()
	total, idle := readCPU()
	if dt := total - m.prevCPUTot; total > m.prevCPUTot && dt > 0 {
		di := idle - m.prevCPUIdl
		m.cpuPct = float64(dt-di) / float64(dt) * 100
	}
	m.prevCPUTot, m.prevCPUIdl = total, idle
}

// visibleRows calcule combien de lignes de process peuvent être affichées, en
// retranchant de la hauteur du terminal la place occupée par le reste de
// l'interface (en-tête, journal, pied de page). Plafonné par maxRows si défini.
func (m model) visibleRows() int {
	const fallback = 15 // tant que la hauteur du terminal est inconnue
	v := fallback
	if m.height > 0 {
		// Lignes occupées par tout sauf les lignes de process :
		//   en-tête encadré (6) + ligne vide (1) + en-tête de colonnes (1) +
		//   ligne vide finale du tableau (1) + ligne d'état (1) +
		//   séparateur du journal (1) + barre système (1) + pied de page (1) +
		//   marge (1).
		overhead := 6 + 1 + 1 + 1 + 1 + 1 + 1 + 1 + 1
		if len(m.events) > 0 {
			overhead += len(m.events) + 2 // titre + ligne vide du journal
		}
		v = m.height - overhead
	}
	if v < 1 {
		v = 1
	}
	if maxRows > 0 && v > maxRows {
		v = maxRows
	}
	return v
}

// clampScroll ramène l'offset de défilement dans les bornes valides.
func (m *model) clampScroll() {
	maxStart := max(0, len(m.rows)-m.visibleRows())
	m.scroll = max(0, min(m.scroll, maxStart))
}

// clampCursor garde l'index sélectionné dans les bornes de la liste.
func (m *model) clampCursor() {
	m.cursor = max(0, min(m.cursor, max(0, len(m.rows)-1)))
}

// ensureVisible ajuste le défilement pour que la ligne sélectionnée reste
// visible dans la fenêtre.
func (m *model) ensureVisible() {
	v := m.visibleRows()
	if m.cursor < m.scroll {
		m.scroll = m.cursor
	}
	if m.cursor >= m.scroll+v {
		m.scroll = m.cursor - v + 1
	}
	m.clampScroll()
}

// selectedPID renvoie le PID de la ligne sélectionnée, ou 0 si la liste est vide.
func (m model) selectedPID() int {
	if m.cursor >= 0 && m.cursor < len(m.rows) {
		return m.rows[m.cursor].pid
	}
	return 0
}

func (m model) Init() tea.Cmd { return tea.Batch(scan, uiTick()) }

// checkNotifications détecte les process qui viennent de franchir le seuil de
// notification (à la hausse) et renvoie les messages à afficher. Met à jour
// l'ensemble m.notified pour ne notifier qu'une fois par franchissement : un
// process repassé sous le seuil pourra re-notifier plus tard.
func (m *model) checkNotifications() []string {
	if !notifyEnabled {
		return nil
	}
	frac := notifyFrac()
	seen := make(map[int]bool)
	var msgs []string
	for _, r := range m.rows {
		if float64(r.rss)/float64(r.limit) < frac {
			continue
		}
		seen[r.pid] = true
		if m.notified[r.pid] {
			continue // déjà notifié tant qu'il reste au-dessus du seuil
		}
		pct := int(float64(r.rss) / float64(r.limit) * 100)
		msgs = append(msgs, fmt.Sprintf("%s (PID %d) atteint %d%% du seuil : %s / %s",
			progLabel(r), r.pid, pct, human(r.rss), human(r.limit)))
	}
	m.notified = seen // oublie les PID repassés sous le seuil (ou disparus)
	return msgs
}

// addEvents ajoute des lignes au journal en respectant la limite maxEvents.
func (m *model) addEvents(evs []string) {
	if len(evs) == 0 {
		return
	}
	m.events = append(m.events, evs...)
	if len(m.events) > maxEvents {
		m.events = m.events[len(m.events)-maxEvents:]
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		key := msg.String()
		// Ctrl+C quitte en toutes circonstances.
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		// Mode confirmation d'un kill manuel : on n'attend qu'un oui/non.
		if m.confirming {
			switch key {
			case "y", "o", "enter":
				pid, name := m.confirmPID, m.confirmName
				m.confirming = false
				return m, killCmd(pid, name)
			default: // n, esc, ou n'importe quelle autre touche : annulation
				m.confirming = false
				return m, nil
			}
		}
		switch key {
		case "q", "esc":
			return m, tea.Quit
		case " ", "r":
			return m, scan // rafraîchir immédiatement
		case "up", "k":
			m.cursor--
			m.clampCursor()
			m.ensureVisible()
			return m, nil
		case "down", "j":
			m.cursor++
			m.clampCursor()
			m.ensureVisible()
			return m, nil
		case "pgup", "b":
			m.cursor -= m.visibleRows()
			m.clampCursor()
			m.ensureVisible()
			return m, nil
		case "pgdown", "f":
			m.cursor += m.visibleRows()
			m.clampCursor()
			m.ensureVisible()
			return m, nil
		case "home", "g":
			m.cursor = 0
			m.ensureVisible()
			return m, nil
		case "end", "G":
			m.cursor = len(m.rows) - 1
			m.clampCursor()
			m.ensureVisible()
			return m, nil
		case "x", "delete":
			// Demande de confirmation d'arrêt du process sélectionné.
			if pid := m.selectedPID(); pid != 0 {
				m.confirming = true
				m.confirmPID = pid
				m.confirmName = progLabel(m.rows[m.cursor])
			}
			return m, nil
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ensureVisible()
		return m, nil
	case scanMsg:
		// Conserve la sélection sur le même PID malgré le ré-ordonnancement.
		selPID := m.selectedPID()
		m.rows = msg.rows
		m.scans++
		now := time.Now()
		m.lastScan = now
		m.nextScan = now.Add(interval)
		if selPID != 0 {
			for i, r := range m.rows {
				if r.pid == selPID {
					m.cursor = i
					break
				}
			}
		}
		m.clampCursor()
		m.ensureVisible()
		m.addEvents(msg.events)
		// Notifie les process qui viennent d'atteindre le seuil d'alerte, et en
		// garde une trace dans le journal.
		notifs := m.checkNotifications()
		next := tea.Tick(interval, func(t time.Time) tea.Msg { return tickMsg(t) })
		if len(notifs) > 0 {
			ts := now.Format("15:04:05")
			evs := make([]string, len(notifs))
			for i, msg := range notifs {
				evs[i] = ts + "  ⚠ " + msg
			}
			m.addEvents(evs)
			return m, tea.Batch(next, notifyCmd(notifs))
		}
		return m, next
	case killMsg:
		m.addEvents(msg.events)
		return m, scan // rafraîchit la liste immédiatement après l'arrêt
	case tickMsg:
		return m, scan
	case uiTickMsg:
		// Rafraîchit l'affichage (compte à rebours) et les stats système.
		m.refreshSysStats()
		return m, uiTick()
	}
	return m, nil
}

// ─── Styles ─────────────────────────────────────────────────────────────────

var (
	cBorder = lipgloss.Color("39") // cyan
	cDim    = lipgloss.Color("240")
	cText   = lipgloss.Color("252")
	cOk     = lipgloss.Color("42")  // vert
	cWarn   = lipgloss.Color("214") // orange
	cBad    = lipgloss.Color("196") // rouge

	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(cBorder)
	dimStyle   = lipgloss.NewStyle().Foreground(cDim)
	headStyle  = lipgloss.NewStyle().Bold(true).Foreground(cDim)
	boxStyle   = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(cBorder).
			Padding(0, 2)
)

// clip tronque une chaîne (sans séquences ANSI) à w colonnes, avec une ellipse
// si elle dépasse. w <= 0 renvoie une chaîne vide.
func clip(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 0 {
		return ""
	}
	if w == 1 {
		return "…"
	}
	return string(r[:w-1]) + "…"
}

// human formate des octets en Ko / Mo / Go.
func human(b uint64) string {
	f := float64(b)
	switch {
	case f >= 1<<30:
		return fmt.Sprintf("%.2f Go", f/(1<<30))
	case f >= 1<<20:
		return fmt.Sprintf("%.0f Mo", f/(1<<20))
	default:
		return fmt.Sprintf("%.0f Ko", f/(1<<10))
	}
}

// progLabel renvoie un nom court et lisible pour un process : le nom de base de
// l'exécutable, ou à défaut celui du motif, tronqué pour tenir dans la colonne.
func progLabel(r procRow) string {
	name := filepath.Base(r.name)
	if name == "" || name == "." || name == "/" {
		name = filepath.Base(r.pattern)
	}
	const max = 16
	if len(name) > max {
		name = name[:max-1] + "…"
	}
	return name
}

// bar dessine une jauge colorée selon le taux de remplissage.
func bar(frac float64) string {
	if frac > 1 {
		frac = 1
	}
	filled := int(frac * float64(barWidth))
	color := cOk
	switch {
	case frac >= 1:
		color = cBad
	case frac >= 0.8:
		color = cWarn
	}
	full := lipgloss.NewStyle().Foreground(color).Render(strings.Repeat("█", filled))
	rest := dimStyle.Render(strings.Repeat("░", barWidth-filled))
	return full + rest
}

func (m model) View() string {
	// En-tête encadré.
	up := time.Since(m.start).Truncate(time.Second)
	last, next := "—", "—"
	if !m.lastScan.IsZero() {
		last = m.lastScan.Format("15:04:05")
	}
	if !m.nextScan.IsZero() {
		countdown := time.Until(m.nextScan).Truncate(time.Second)
		if countdown < 0 {
			countdown = 0
		}
		next = fmt.Sprintf("%s (dans %s)", m.nextScan.Format("15:04:05"), countdown)
	}
	cibleLabel := "cible     : "
	if len(patterns) > 1 {
		cibleLabel = "cibles    : "
	}
	// Chaque cible affiche son motif, annoté de son seuil propre s'il diffère
	// du seuil par défaut.
	parts := make([]string, len(patterns))
	for i, p := range patterns {
		if lim, ok := perPatternLimit[p]; ok && lim != limitBytes {
			parts[i] = fmt.Sprintf("%s (%s)", p, human(lim))
		} else {
			parts[i] = p
		}
	}
	seuilLabel := "seuil     : "
	if len(perPatternLimit) > 0 {
		seuilLabel = "seuil déf.: "
	}
	// Largeur utile du contenu de l'en-tête (cadre plein écran : largeur du
	// terminal moins bordure (2) et padding (4)). Les lignes sont tronquées à
	// cette largeur pour ne jamais se replier (ce qui casserait la hauteur).
	contentW := 1 << 30
	if m.width > 6 {
		contentW = m.width - 6
	}
	header := lipgloss.JoinVertical(
		lipgloss.Left,
		titleStyle.Render("Memory Watchdog"),
		dimStyle.Render(clip(cibleLabel+strings.Join(parts, ", "), contentW)),
		dimStyle.Render(clip(fmt.Sprintf("%s%s   ·   intervalle : %s   ·   uptime : %s   ·   scans : %d",
			seuilLabel, human(limitBytes), interval, up, m.scans), contentW)),
		dimStyle.Render(clip(fmt.Sprintf("dernier   : %s   ·   prochain   : %s", last, next), contentW)),
	)

	// Tableau (fenêtre de défilement).
	total := len(m.rows)
	visible := m.visibleRows()
	start := max(0, min(m.scroll, max(0, total-visible)))
	end := min(total, start+visible)

	var b strings.Builder
	b.WriteString(headStyle.Render("  PID      PROGRAMME         MÉMOIRE       USAGE                            ÉTAT") + "\n")
	if total == 0 {
		b.WriteString(dimStyle.Render("  (aucun process ne correspond aux motifs)") + "\n")
	}
	cursorStyle := lipgloss.NewStyle().Foreground(cBorder).Bold(true)
	for i, r := range m.rows[start:end] {
		frac := float64(r.rss) / float64(r.limit)
		pct := int(frac * 100)
		if pct > 100 {
			pct = 100
		}

		memColor, state := cText, lipgloss.NewStyle().Foreground(cOk).Render("● ok")
		switch {
		case r.over:
			memColor = cBad
			state = lipgloss.NewStyle().Foreground(cBad).Bold(true).Render("✖ dépassement")
		case frac >= 0.8:
			memColor = cWarn
			state = lipgloss.NewStyle().Foreground(cWarn).Render("▲ proche")
		}

		// Champs à largeur fixe (formatés avant toute coloration pour préserver
		// l'alignement, la coloration ajoutant des séquences ANSI invisibles).
		pidField := fmt.Sprintf("%-7d", r.pid)
		progField := fmt.Sprintf("%-16s", progLabel(r))
		mem := lipgloss.NewStyle().Foreground(memColor).Render(fmt.Sprintf("%-12s", human(r.rss)))

		marker := "  "
		if start+i == m.cursor {
			marker = cursorStyle.Render("▸") + " "
			pidField = cursorStyle.Render(pidField)
			progField = cursorStyle.Render(progField)
		}
		b.WriteString(fmt.Sprintf("%s%s  %s  %s  %s %4d%%   %s\n", marker, pidField, progField, mem, bar(frac), pct, state))
	}

	// Ligne d'état du tableau : position de défilement et lignes masquées.
	switch {
	case total > visible:
		above, below := start, total-end
		arrows := ""
		if above > 0 {
			arrows += "↑"
		}
		if below > 0 {
			arrows += "↓"
		}
		b.WriteString(dimStyle.Render(fmt.Sprintf("  %s  lignes %d–%d / %d   ·   %d masquées",
			arrows, start+1, end, total, above+below)) + "\n")
	case total > 0:
		b.WriteString(dimStyle.Render(fmt.Sprintf("  %d process", total)) + "\n")
	default:
		b.WriteString("\n") // ligne d'état vide : hauteur stable pour visibleRows
	}

	// Journal.
	log := ""
	if len(m.events) > 0 {
		var lb strings.Builder
		lb.WriteString("\n" + headStyle.Render("  Journal") + "\n")
		for _, e := range m.events {
			lb.WriteString(dimStyle.Render("  "+e) + "\n")
		}
		log = lb.String()
	}

	// Barre système : RAM et CPU totaux de la machine, + RAM cumulée des
	// process surveillés.
	var sumRSS uint64
	for _, r := range m.rows {
		sumRSS += r.rss
	}
	memPct := 0
	if m.memTotal > 0 {
		memPct = int(float64(m.memUsed) / float64(m.memTotal) * 100)
	}
	// Couleur selon la charge : vert < 75 % < orange < 90 % < rouge.
	loadColor := func(p int) lipgloss.Color {
		switch {
		case p >= 90:
			return cBad
		case p >= 75:
			return cWarn
		default:
			return cOk
		}
	}
	val := func(s string, c lipgloss.Color) string {
		return lipgloss.NewStyle().Foreground(c).Bold(true).Render(s)
	}
	sep := dimStyle.Render("   ·   ")
	sysbar := "  " +
		dimStyle.Render("RAM système : ") +
		val(fmt.Sprintf("%s / %s", human(m.memUsed), human(m.memTotal)), cText) + " " +
		val(fmt.Sprintf("(%d%%)", memPct), loadColor(memPct)) + sep +
		dimStyle.Render("CPU système : ") +
		val(fmt.Sprintf("%.0f%%", m.cpuPct), loadColor(int(m.cpuPct))) + sep +
		dimStyle.Render("surveillés : ") +
		val(human(sumRSS), cBorder)

	var footer string
	if m.confirming {
		prompt := lipgloss.NewStyle().Foreground(cBad).Bold(true).Render(
			fmt.Sprintf("  Tuer le process PID %d (%s) ?", m.confirmPID, m.confirmName))
		footer = prompt + dimStyle.Render("   y/o = confirmer · n/Échap = annuler")
	} else {
		footer = dimStyle.Render("  q quitter · espace/r rafraîchir · ↑/↓ sélection · x tuer · PgUp/PgDn page · g/G début/fin")
	}

	// Étire le cadre de l'en-tête sur toute la largeur du terminal.
	box := boxStyle
	if m.width > 6 {
		box = box.Width(m.width - 2) // + bordure (2) = largeur du terminal
	}

	out := lipgloss.JoinVertical(
		lipgloss.Left,
		box.Render(header),
		"",
		b.String(),
		log,
		sysbar,
		footer,
	)
	// Borne chaque ligne à la largeur du terminal (tronque proprement les
	// lignes trop longues au lieu de les laisser se replier).
	if m.width > 0 {
		out = lipgloss.NewStyle().MaxWidth(m.width).Render(out)
	}
	return out
}

func main() {
	// Priorité : valeurs par défaut < fichier de config < arguments positionnels.
	const defaultConfig = "watchdog.json"
	cfgPath := flag.String("config", defaultConfig, "chemin du fichier de configuration JSON")
	flag.Parse()

	// Le fichier n'est obligatoire que si l'utilisateur l'a explicitement demandé.
	mustExist := *cfgPath != defaultConfig
	if err := loadConfig(*cfgPath, mustExist); err != nil {
		fmt.Fprintln(os.Stderr, "erreur:", err)
		os.Exit(1)
	}

	// Les arguments positionnels restants surchargent les motifs.
	if args := flag.Args(); len(args) > 0 {
		patterns = args
	}

	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "erreur:", err)
		os.Exit(1)
	}
}
