package tui

import (
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/rnapoles/kube-emulator/internal/trace"
)

type changeMsg struct{}
type tickMsg time.Time

// zone is a clickable horizontal range [x0, x1).
type zone struct {
	x0, x1 int
	id     string
}

var (
	methodCycle = []string{"", "GET", "POST", "PUT", "PATCH", "DELETE"}
	statusCycle = []string{"", "2xx", "3xx", "4xx", "5xx"}
)

type model struct {
	opt Options
	rec *trace.Recorder
	w   int
	h   int

	all        []trace.Exchange
	view       []int // indices into all that pass the filters
	cursor     int   // index into view
	top        int   // first visible row of the list
	selID      int64
	shownTotal int64
	pendingNew int

	follow    bool
	paused    bool
	hideNoise bool
	method    string
	status    string
	search    textinput.Model
	searching bool
	terms     []term

	tab       int
	focus     int // 0 list, 1 details
	detail    []string
	dy        int
	detailKey string
	detailSel string

	showHelp bool
	showLogs bool

	toast      string
	toastUntil time.Time
	tick       int

	chipZones []zone
	tabZones  []zone
}

func newModel(opt Options) *model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = "filter…   pods   status:4xx   -verb:watch   body:nginx"
	ti.CharLimit = 160
	ti.TextStyle = stText
	ti.PlaceholderStyle = stDim
	m := &model{opt: opt, rec: opt.Rec, follow: true, search: ti}
	m.reload()
	return m
}

func (m *model) Init() tea.Cmd { return tea.Batch(waitChange(m.rec), tickCmd()) }

func waitChange(r *trace.Recorder) tea.Cmd {
	return func() tea.Msg {
		<-r.Changed()
		time.Sleep(40 * time.Millisecond) // coalesce bursts
		return changeMsg{}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// ---------------------------------------------------------------- geometry

func (m *model) bodyH() int { return m.h - 4 }
func (m *model) leftW() int {
	lw := m.w * 45 / 100
	if lw < 40 {
		lw = 40
	}
	if lw > m.w-34 {
		lw = m.w - 34
	}
	return lw
}
func (m *model) listRows() int       { return max(1, m.bodyH()-3) }
func (m *model) detailRows() int     { return max(1, m.bodyH()-3) }
func (m *model) detailW() int        { return max(10, m.w-m.leftW()-4) }
func (m *model) cols(iw int) (bool, int) {
	if iw-30 >= 12 {
		return true, iw - 30
	}
	return false, max(4, iw-21)
}

// ---------------------------------------------------------------- data

func (m *model) selected() *trace.Exchange {
	if m.cursor < 0 || m.cursor >= len(m.view) {
		return nil
	}
	return &m.all[m.view[m.cursor]]
}

func (m *model) match(ex *trace.Exchange) bool {
	if m.hideNoise && ex.Noise {
		return false
	}
	if m.method != "" && ex.Method != m.method {
		return false
	}
	if m.status != "" && !statusMatches(ex.Status, m.status) {
		return false
	}
	for _, t := range m.terms {
		if !t.match(ex) {
			return false
		}
	}
	return true
}

// reload takes a fresh snapshot of the recorder.
func (m *model) reload() {
	m.all = m.rec.Snapshot()
	m.shownTotal = m.rec.Total()
	m.pendingNew = 0
	m.applyFilter()
}

// refresh reacts to new traffic; while paused it only counts it.
func (m *model) refresh() {
	if m.paused {
		m.pendingNew = int(m.rec.Total() - m.shownTotal)
		return
	}
	m.reload()
}

func (m *model) applyFilter() {
	m.view = m.view[:0]
	for i := range m.all {
		if m.match(&m.all[i]) {
			m.view = append(m.view, i)
		}
	}
	idx := -1
	if !m.follow {
		for i, vi := range m.view {
			if m.all[vi].ID == m.selID {
				idx = i
				break
			}
		}
	}
	switch {
	case len(m.view) == 0:
		m.cursor = 0
	case m.follow:
		m.cursor = len(m.view) - 1
	case idx >= 0:
		m.cursor = idx
	case m.cursor >= len(m.view):
		m.cursor = len(m.view) - 1
	}
	m.syncSelection()
}

func (m *model) syncSelection() {
	if ex := m.selected(); ex != nil {
		m.selID = ex.ID
	} else {
		m.selID = 0
	}
	m.ensureVisible()
	m.rebuildDetail(false)
}

func (m *model) ensureVisible() {
	rows := m.listRows()
	if m.cursor < m.top {
		m.top = m.cursor
	}
	if m.cursor >= m.top+rows {
		m.top = m.cursor - rows + 1
	}
	if limit := len(m.view) - rows; m.top > limit {
		m.top = limit
	}
	if m.top < 0 {
		m.top = 0
	}
}

func (m *model) rebuildDetail(force bool) {
	ex := m.selected()
	if ex == nil || m.w == 0 {
		m.detail, m.detailKey = nil, ""
		return
	}
	w := m.detailW()
	bucket := 0
	if !ex.Done {
		bucket = m.tick/4 + 1 // in-flight requests refresh about once a second
	}
	key := fmt.Sprintf("%d|%d|%d|%d|%d|%d|%d|%d", ex.ID, m.tab, w, bucket, ex.Status, len(ex.RespBody), len(ex.ReqBody), len(ex.RespHeader))
	if key == m.detailKey && !force {
		return
	}
	if sel := fmt.Sprintf("%d|%d", ex.ID, m.tab); sel != m.detailSel {
		m.dy, m.detailSel = 0, sel
	}
	m.detailKey = key
	m.detail = detailLines(ex, m.tab, w)
	m.clampDetail()
}

func (m *model) clampDetail() {
	limit := len(m.detail) - m.detailRows()
	if m.dy > limit {
		m.dy = limit
	}
	if m.dy < 0 {
		m.dy = 0
	}
}

func (m *model) setToast(s string) {
	m.toast, m.toastUntil = s, time.Now().Add(2500*time.Millisecond)
}

// ---------------------------------------------------------------- update

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.syncSelection()
		m.rebuildDetail(true)
		return m, nil
	case changeMsg:
		m.refresh()
		return m, waitChange(m.rec)
	case tickMsg:
		m.tick++
		m.rebuildDetail(false)
		return m, tickCmd()
	case tea.KeyMsg:
		return m.onKey(msg)
	case tea.MouseMsg:
		return m.onMouse(msg)
	}
	return m, nil
}

func (m *model) cycle(list []string, cur string) string {
	for i, v := range list {
		if v == cur {
			return list[(i+1)%len(list)]
		}
	}
	return ""
}

func (m *model) act(id string) tea.Cmd {
	switch id {
	case "method":
		m.method = m.cycle(methodCycle, m.method)
		m.applyFilter()
	case "status":
		m.status = m.cycle(statusCycle, m.status)
		m.applyFilter()
	case "noise":
		m.hideNoise = !m.hideNoise
		m.applyFilter()
	case "pause":
		m.paused = !m.paused
		if !m.paused {
			m.reload()
		}
	case "follow":
		m.follow = !m.follow
		if m.follow {
			m.applyFilter()
		}
	case "clear":
		m.rec.Clear()
		m.reload()
		m.setToast("cleared")
	case "help":
		m.showHelp = true
	case "search":
		m.searching = true
		return m.search.Focus()
	}
	return nil
}

func (m *model) resetFilters() {
	m.method, m.status, m.hideNoise = "", "", false
	m.search.SetValue("")
	m.terms = nil
	m.applyFilter()
}

func (m *model) setTab(i int) {
	m.tab = (i + numTabs) % numTabs
	m.rebuildDetail(false)
}

func (m *model) move(delta int) {
	if m.focus == 1 {
		m.dy += delta
		m.clampDetail()
		return
	}
	n := len(m.view)
	if n == 0 {
		return
	}
	m.cursor = min(max(m.cursor+delta, 0), n-1)
	m.follow = m.cursor == n-1
	m.syncSelection()
}

func (m *model) jump(end bool) {
	if m.focus == 1 {
		m.dy = 0
		if end {
			m.dy = len(m.detail)
		}
		m.clampDetail()
		return
	}
	if n := len(m.view); n > 0 {
		m.cursor = 0
		if end {
			m.cursor = n - 1
		}
		m.follow = end
		m.syncSelection()
	}
}

func (m *model) jumpError(dir int) {
	n := len(m.view)
	for step := 1; step <= n; step++ {
		i := ((m.cursor+dir*step)%n + n) % n
		if m.all[m.view[i]].Status >= 400 {
			m.cursor, m.follow = i, false
			m.syncSelection()
			return
		}
	}
	m.setToast("no errors in the current view")
}

func (m *model) onKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := k.String()
	if s == "ctrl+c" {
		return m, tea.Quit
	}
	if m.searching {
		switch s {
		case "esc", "enter":
			m.searching = false
			m.search.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(k)
		m.terms = parseTerms(m.search.Value())
		m.applyFilter()
		return m, cmd
	}
	if m.showHelp || m.showLogs {
		switch s {
		case "esc", "q", "?", "l", "enter":
			m.showHelp, m.showLogs = false, false
		}
		return m, nil
	}
	page := m.listRows()
	if m.focus == 1 {
		page = m.detailRows()
	}
	switch s {
	case "q":
		return m, tea.Quit
	case "?":
		return m, m.act("help")
	case "l":
		m.showLogs = true
	case "/":
		return m, m.act("search")
	case "esc":
		if m.search.Value() != "" {
			m.search.SetValue("")
			m.terms = nil
			m.applyFilter()
		}
	case "tab", "shift+tab":
		m.focus = 1 - m.focus
	case "left":
		m.focus = 0
	case "right":
		m.focus = 1
	case "1", "2", "3":
		n, _ := strconv.Atoi(s)
		m.setTab(n - 1)
	case "[":
		m.setTab(m.tab - 1)
	case "]":
		m.setTab(m.tab + 1)
	case "m":
		return m, m.act("method")
	case "s":
		return m, m.act("status")
	case "d":
		return m, m.act("noise")
	case "p":
		return m, m.act("pause")
	case "f":
		return m, m.act("follow")
	case "c":
		return m, m.act("clear")
	case "x":
		m.resetFilters()
		m.setToast("filters reset")
	case "e":
		m.jumpError(1)
	case "E":
		m.jumpError(-1)
	case "y":
		return m, m.copyCurl()
	case "Y":
		return m, m.copyBody()
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "pgup", "ctrl+b":
		m.move(-page)
	case "pgdown", "ctrl+f", " ":
		m.move(page)
	case "home", "g":
		m.jump(false)
	case "end", "G":
		m.jump(true)
	}
	return m, nil
}

func (m *model) onMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.showHelp || m.showLogs {
		if msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress {
			m.showHelp, m.showLogs = false, false
		}
		return m, nil
	}
	lw := m.leftW()
	switch {
	case msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown:
		d := 3
		if msg.Button == tea.MouseButtonWheelUp {
			d = -3
		}
		if msg.X < lw {
			m.focus = 0
		} else {
			m.focus = 1
		}
		m.move(d)
	case msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress:
		switch {
		case msg.Y == 1:
			for _, z := range m.chipZones {
				if msg.X >= z.x0 && msg.X < z.x1 {
					return m, m.act(z.id)
				}
			}
		case msg.Y >= 3 && msg.Y < 3+m.bodyH():
			if msg.X < lw {
				m.focus = 0
				row := msg.Y - 5
				if row >= 0 && row < m.listRows() && m.top+row < len(m.view) {
					m.cursor = m.top + row
					m.follow = m.cursor == len(m.view)-1
					m.syncSelection()
				}
			} else {
				m.focus = 1
				if msg.Y == 4 {
					for _, z := range m.tabZones {
						if msg.X >= z.x0 && msg.X < z.x1 {
							n, _ := strconv.Atoi(z.id)
							m.setTab(n)
						}
					}
				}
			}
		}
	}
	return m, nil
}

// ---------------------------------------------------------------- clipboard

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func curlCommand(ex *trace.Exchange, base string) string {
	u := base + ex.Path
	if ex.RawQuery != "" {
		u += "?" + ex.RawQuery
	}
	var sb strings.Builder
	sb.WriteString("curl -sS -X " + ex.Method + " " + shq(u))
	keys := make([]string, 0, len(ex.ReqHeader))
	for k := range ex.ReqHeader {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case "Content-Type", "Accept", "Authorization":
			sb.WriteString(" -H " + shq(k+": "+strings.Join(ex.ReqHeader[k], ", ")))
		}
	}
	if len(ex.ReqBody) > 0 && !ex.ReqTruncated {
		sb.WriteString(" --data-binary " + shq(string(ex.ReqBody)))
	}
	return sb.String()
}

// osc52 copies text through the terminal (supported by most modern terminals).
func osc52(s string) tea.Cmd {
	return func() tea.Msg {
		if len(s) > 90000 {
			s = s[:90000]
		}
		_, _ = os.Stdout.WriteString("\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(s)) + "\x07")
		return nil
	}
}

func (m *model) copyCurl() tea.Cmd {
	ex := m.selected()
	if ex == nil {
		m.setToast("nothing selected")
		return nil
	}
	m.setToast("curl command copied (needs a terminal with OSC 52)")
	return osc52(curlCommand(ex, m.opt.URL))
}

func (m *model) copyBody() tea.Cmd {
	ex := m.selected()
	if ex == nil || len(ex.RespBody) == 0 {
		m.setToast("no response body to copy")
		return nil
	}
	body, _ := prettyJSON(ex.RespBody)
	m.setToast("response body copied (needs a terminal with OSC 52)")
	return osc52(body)
}

// ---------------------------------------------------------------- view

func (m *model) View() string {
	if m.w == 0 {
		return ""
	}
	if m.w < 64 || m.h < 14 {
		return lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center,
			stAmber.Render("terminal too small")+"\n"+stDim.Render("needs at least 64 x 14"))
	}
	now := time.Now()
	head := m.renderHeader(now)
	var body string
	switch {
	case m.showHelp:
		body = m.renderHelp()
	case m.showLogs:
		body = m.renderLogs()
	default:
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.renderList(now), m.renderDetail())
	}
	return strings.Join(append(head, body, m.renderFooter()), "\n")
}

type stats struct {
	total                            int64
	ok, redirect, clientErr, srvErr  int
	inflight                         int
	avg                              time.Duration
	perSec                           []int
	rate                             float64
}

func (m *model) computeStats(now time.Time) stats {
	s := stats{total: m.rec.Total(), perSec: make([]int, 20)}
	var sum time.Duration
	var n int
	for i := range m.all {
		ex := &m.all[i]
		switch {
		case ex.Status == 0:
			s.inflight++
		case ex.Status < 300:
			s.ok++
		case ex.Status < 400:
			s.redirect++
		case ex.Status < 500:
			s.clientErr++
		default:
			s.srvErr++
		}
		if ex.Done && !ex.Streaming {
			sum += ex.Duration
			n++
		}
		if age := int(now.Sub(ex.Started) / time.Second); age >= 0 && age < 20 {
			s.perSec[19-age]++
		}
	}
	if n > 0 {
		s.avg = sum / time.Duration(n)
	}
	last := 0
	for _, v := range s.perSec[15:] {
		last += v
	}
	s.rate = float64(last) / 5
	return s
}

func (m *model) renderHeader(now time.Time) []string {
	title := gradientText(" ◆ KUBE·EMULATOR ", gradFrom, gradTo, true) + stDim.Render(" API traffic inspector")
	state := ""
	if m.paused {
		state = stAmber.Bold(true).Render("❚❚ PAUSED")
	} else {
		dot := "●"
		if m.tick%4 >= 2 {
			dot = "○"
		}
		state = stGreen.Bold(true).Render(dot + " LIVE")
	}
	right := state + "  " + stCyan.Render(m.opt.URL) + " "
	gap := max(1, m.w-lipgloss.Width(title)-lipgloss.Width(right))
	l1 := title + strings.Repeat(" ", gap) + right
	return []string{fit(l1, m.w), fit(m.renderToolbar(), m.w), fit(m.renderStats(now), m.w)}
}

type chip struct {
	id, key, label, value string
	active                bool
}

func (m *model) chips() []chip {
	or := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	disc := "shown"
	if m.hideNoise {
		disc = "hidden"
	}
	return []chip{
		{"method", "m", "METHOD", or(m.method, "ALL"), m.method != ""},
		{"status", "s", "STATUS", or(m.status, "ALL"), m.status != ""},
		{"noise", "d", "DISCOVERY", disc, m.hideNoise},
		{"pause", "p", "PAUSE", onOff(m.paused), m.paused},
		{"follow", "f", "FOLLOW", onOff(m.follow), m.follow},
		{"clear", "c", "CLEAR", "", false},
		{"help", "?", "HELP", "", false},
	}
}

func renderChip(c chip, compact bool) string {
	key := lipgloss.NewStyle().Background(lipgloss.Color(cViolet)).Foreground(lipgloss.Color(cBadgeFg)).Bold(true).Render(" " + c.key + " ")
	bg := lipgloss.Color(cChipBg)
	text := c.label
	if c.value != "" {
		text += " "
	}
	if compact && c.value != "" {
		text = ""
	}
	lab := lipgloss.NewStyle().Background(bg).Foreground(lipgloss.Color(cSubtle)).Render(" " + text)
	valColor := cText
	if c.active {
		valColor = cGreen
	}
	val := lipgloss.NewStyle().Background(bg).Foreground(lipgloss.Color(valColor)).Bold(c.active).Render(c.value + " ")
	if c.value == "" {
		val = lipgloss.NewStyle().Background(bg).Render(" ")
	}
	return key + lab + val
}

func (m *model) renderToolbar() string {
	chips := m.chips()
	build := func(compact bool) (string, []zone, int) {
		var sb strings.Builder
		var zones []zone
		x := 1
		sb.WriteString(" ")
		for _, c := range chips {
			s := renderChip(c, compact)
			w := lipgloss.Width(s)
			zones = append(zones, zone{x, x + w, c.id})
			sb.WriteString(s + " ")
			x += w + 1
		}
		return sb.String(), zones, x
	}
	line, zones, x := build(false)
	if m.w-x < 26 {
		line, zones, x = build(true)
	}
	boxW := max(10, m.w-x-1)
	m.search.Width = max(4, boxW-3)
	icon := stDim.Render("⌕ ")
	if m.searching {
		icon = fg(cMagenta).Bold(true).Render("⌕ ")
	}
	input := m.search.View()
	if !m.searching && m.search.Value() != "" {
		input = fg(cGreen).Render(m.search.Value())
	}
	zones = append(zones, zone{x, m.w, "search"})
	m.chipZones = zones
	return line + " " + icon + ansi.Truncate(input, boxW-2, "")
}

func (m *model) renderStats(now time.Time) string {
	s := m.computeStats(now)
	sep := stDim.Render("  │  ")
	parts := []string{
		" " + sparkline(s.perSec) + " " + stBold.Render(fmt.Sprintf("%.1f", s.rate)) + stDim.Render(" req/s"),
		stText.Render(strconv.FormatInt(s.total, 10)) + stDim.Render(" total"),
		stGreen.Render(fmt.Sprintf("✓ %d", s.ok+s.redirect)),
		stAmber.Render(fmt.Sprintf("⚠ %d", s.clientErr)),
		stRed.Render(fmt.Sprintf("✗ %d", s.srvErr)),
		stDim.Render("avg ") + stText.Render(fmtDur(s.avg)),
	}
	if s.inflight > 0 {
		parts = append(parts, fg(cMagenta).Render(fmt.Sprintf("◌ %d in flight", s.inflight)))
	}
	left := strings.Join(parts, sep)
	right := stDim.Render("showing ") + stText.Render(fmt.Sprintf("%d", len(m.view))) + stDim.Render(fmt.Sprintf(" of %d ", len(m.all)))
	gap := max(1, m.w-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

func (m *model) renderFooter() string {
	hints := [][2]string{{"↑↓", "move"}, {"⇥", "pane"}, {"1-3", "tabs"}, {"/", "search"}, {"p", "pause"}, {"e", "next error"}, {"y", "curl"}, {"l", "logs"}, {"?", "help"}, {"q", "quit"}}
	parts := make([]string, len(hints))
	for i, h := range hints {
		parts[i] = fg(cViolet).Bold(true).Render(h[0]) + " " + stDim.Render(h[1])
	}
	left := " " + strings.Join(parts, "  ")
	if time.Now().Before(m.toastUntil) {
		left = " " + stAmber.Bold(true).Render("✓ "+m.toast)
	}
	right := ""
	if m.pendingNew > 0 {
		right += stAmber.Render(fmt.Sprintf("+%d new  ", m.pendingNew))
	}
	if n := len(m.view); n > 0 {
		right += stSubtle.Render(fmt.Sprintf("%d/%d ", m.cursor+1, n))
	}
	room := m.w - lipgloss.Width(right)
	left = ansi.Truncate(left, room-1, "")
	return fit(left+strings.Repeat(" ", max(1, room-lipgloss.Width(left)))+right, m.w)
}

func (m *model) renderList(now time.Time) string {
	lw := m.leftW()
	iw := lw - 2
	showTime, flex := m.cols(iw)
	head := "  "
	if showTime {
		head += padRight("WHEN", 9)
	}
	head += padRight("METHOD", 7) + padRight("ST", 4) + padRight("REQUEST", flex) + padLeft("TOOK", 8)
	lines := []string{stDim.Bold(true).Render(head)}
	if len(m.view) == 0 {
		lines = append(lines, "")
		if len(m.all) == 0 {
			lines = append(lines, stSubtle.Render("  No requests yet."), "",
				stDim.Render("  Point kubectl at the emulator:"),
				"  "+stCyan.Render("kubectl --server="+m.opt.URL+" get ns"))
		} else {
			lines = append(lines, stSubtle.Render("  Nothing matches the filters."), stDim.Render("  Press x to reset them."))
		}
	}
	for i := m.top; i < len(m.view) && i < m.top+m.listRows(); i++ {
		lines = append(lines, m.renderRow(&m.all[m.view[i]], i == m.cursor, showTime, flex, now))
	}
	title := stBold.Render("⟡ Requests") + stDim.Render(fmt.Sprintf("  %d", len(m.view)))
	footer := ""
	if n := len(m.view); n > 0 {
		footer = stDim.Render(fmt.Sprintf("%d/%d", m.cursor+1, n))
	}
	return box(title, footer, lines, lw, m.bodyH(), m.focus == 0)
}

func (m *model) renderRow(ex *trace.Exchange, sel, showTime bool, flex int, now time.Time) string {
	seg := func(txt, color string, bold bool) string {
		st := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Bold(bold)
		if sel {
			st = st.Background(lipgloss.Color(cSelBg))
		}
		return st.Render(txt)
	}
	var b strings.Builder
	bar := "▏"
	if sel {
		bar = "▍"
	}
	b.WriteString(seg(bar, statusColor(ex.Status), true) + seg(" ", cText, false))
	if showTime {
		b.WriteString(seg(ex.Started.Format("15:04:05")+" ", cDim, false))
	}
	b.WriteString(seg(padRight(ex.Method, 6)+" ", methodColor(ex.Method), true))
	st := "···"
	if ex.Status != 0 {
		st = strconv.Itoa(ex.Status)
	}
	b.WriteString(seg(padRight(st, 3)+" ", statusColor(ex.Status), true))
	col := cText
	if ex.Noise {
		col = cDim
	}
	b.WriteString(seg(fit(ex.Summary, flex), col, sel))
	dur, dcol := "", cDim
	switch {
	case ex.Done:
		dur = fmtDur(ex.Duration)
		if ex.Duration > 2*time.Second {
			dcol = cRed
		} else if ex.Duration > 500*time.Millisecond {
			dcol = cAmber
		}
	case ex.Streaming:
		dur, dcol = "● live", cMagenta
	default:
		dur, dcol = fmtDur(now.Sub(ex.Started)), cAmber
	}
	b.WriteString(seg(" "+padLeft(dur, 7), dcol, false))
	return b.String()
}

func (m *model) renderDetail() string {
	lw := m.leftW()
	rw := m.w - lw
	ex := m.selected()

	var tabs strings.Builder
	m.tabZones = nil
	tabs.WriteString(" ")
	x := lw + 2
	for i, name := range tabNames {
		label := fmt.Sprintf(" %d %s ", i+1, name)
		var s string
		if i == m.tab {
			s = lipgloss.NewStyle().Background(lipgloss.Color(cTabBg)).Foreground(lipgloss.Color(cText)).Bold(true).Render(label)
		} else {
			s = stDim.Render(label)
		}
		w := lipgloss.Width(s)
		m.tabZones = append(m.tabZones, zone{x, x + w, strconv.Itoa(i)})
		tabs.WriteString(s)
		x += w
	}
	lines := []string{tabs.String()}
	title := stBold.Render("◈ Details")
	footer := ""
	if ex == nil {
		lines = append(lines, "",
			stSubtle.Render("  Waiting for a request…"), "",
			stDim.Render("  Select a request on the left to inspect"),
			stDim.Render("  its headers, body and response."))
	} else {
		title = stBold.Render("◈ ") + fg(methodColor(ex.Method)).Bold(true).Render(ex.Method) + " " + stSubtle.Render(ex.Summary)
		rows := m.detailRows()
		for i := m.dy; i < len(m.detail) && i < m.dy+rows; i++ {
			lines = append(lines, " "+m.detail[i])
		}
		if len(m.detail) > rows {
			pct := 100
			if limit := len(m.detail) - rows; limit > 0 {
				pct = m.dy * 100 / limit
			}
			footer = stDim.Render(fmt.Sprintf("↑↓ %d%%", pct))
		}
	}
	return box(title, footer, lines, rw, m.bodyH(), m.focus == 1)
}

func keyCol(s string) string { return fg(cCyan).Bold(true).Render(padRight(s, 17)) }

func (m *model) renderHelp() string {
	type group struct {
		title string
		rows  [][2]string
	}
	groups := []group{
		{"NAVIGATE", [][2]string{
			{"↑ ↓  j k", "move in the focused pane"}, {"pgup pgdn  space", "page"}, {"g G  home end", "first / last"},
			{"tab  ← →", "switch pane"}, {"1 2 3  [ ]", "overview / request / response"}, {"mouse", "click rows, tabs and buttons; wheel scrolls"}}},
		{"FILTER", [][2]string{
			{"/", "search (enter or esc to leave the box)"}, {"m", "cycle method"}, {"s", "cycle status class"},
			{"d", "hide or show API discovery noise"}, {"x", "reset all filters"}, {"esc", "clear the search text"}}},
		{"SEARCH SYNTAX", [][2]string{
			{"words", "all words must match (method, path, summary, status)"},
			{"key:value", "method: status: verb: res: ns: name: path: body:"},
			{"-term", "negate, for example  -verb:watch  -status:200"}}},
		{"ACTIONS", [][2]string{
			{"p", "pause or resume the live view"}, {"f", "follow the newest request"}, {"c", "clear the history"},
			{"e  E", "next / previous error (4xx, 5xx)"}, {"y", "copy the request as a curl command"}, {"Y", "copy the response body"},
			{"l", "server log"}, {"q", "quit and stop the emulator"}}},
	}
	var lines []string
	for gi, g := range groups {
		if gi > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, " "+fg(cViolet).Bold(true).Render("▌ "+g.title))
		for _, r := range g.rows {
			lines = append(lines, "   "+keyCol(r[0])+stSubtle.Render(r[1]))
		}
	}
	w := min(86, m.w-4)
	h := min(len(lines)+2, m.bodyH())
	b := box(gradientText("Help", gradFrom, gradTo, true), stDim.Render("esc to close"), lines, w, h, true)
	return lipgloss.Place(m.w, m.bodyH(), lipgloss.Center, lipgloss.Center, b)
}

func (m *model) renderLogs() string {
	var all []string
	if m.opt.Logs != nil {
		all = m.opt.Logs.Lines()
	}
	rows := m.bodyH() - 2
	if len(all) > rows {
		all = all[len(all)-rows:]
	}
	lines := make([]string, 0, len(all))
	for _, l := range all {
		lines = append(lines, " "+stSubtle.Render(l))
	}
	if len(lines) == 0 {
		lines = []string{"", stDim.Render("  (no log output yet)")}
	}
	return box(gradientText("Server log", gradFrom, gradTo, true), stDim.Render("l or esc to close"), lines, m.w, m.bodyH(), true)
}
