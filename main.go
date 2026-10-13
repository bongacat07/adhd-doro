package main

import (
	"bytes"
	"embed"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/mp3"
	"github.com/gopxl/beep/v2/speaker"
)

// ADHD-doro: one doro-session is a series of phases. The first work block
// is -start long and shrinks by -step each phase until it reaches zero;
// every break is -break long. Defaults: 60, 10, 10 (six phases).
//
// Flag values are in multiples of unit. Use time.Minute for real use.
// For quick testing, run with fractions, e.g. -start 0.1 -step 0.01 -break 0.01
// (6s, 5s, 4s ...), or temporarily set unit to time.Second.
const unit = time.Minute

type Phase struct {
	Work  time.Duration
	Break time.Duration
}

// session is built from the flags in main.
var session []Phase

const maxPhases = 100

// buildSession makes phases of work start, start-step, ... > 0,
// each followed by a break of brk. All values are in units.
func buildSession(start, step, brk float64) []Phase {
	var out []Phase
	for w := start; w > 1e-9; w -= step {
		out = append(out, Phase{
			Work:  time.Duration(w * float64(unit)),
			Break: time.Duration(brk * float64(unit)),
		})
	}
	return out
}

// ---------- palette ----------

// Dark jester palette: deep violet, burnt orange, dark gold, emerald, teal.
const (
	violet = "#7C3AED" // deep jester purple
	teal   = "#0D9488" // dark teal
	green  = "#16A34A" // emerald
	gold   = "#CA8A04" // dark gold
	orange = "#EA580C" // burnt orange
	blue   = "#2563EB" // royal blue
	purple = "#6D28D9" // plum
	dim    = "#8B7FA8" // muted lavender
	text   = "#D6CFE8" // soft lilac-white
	track  = "#1E1530" // near-black purple
	yellow = "#EAB308" // paused amber
	shadow = "#2E1A4D" // pixel-art drop shadow
)

// ---------- model ----------

type stage int

const (
	working stage = iota
	onBreak
	done
)

type model struct {
	phaseIdx  int
	stage     stage
	stageDur  time.Duration
	end       time.Time     // when the current stage ends
	remaining time.Duration // frozen value while paused
	paused    bool
	tickID    int // invalidates stale tick loops after pause/restart

	width, height int
}

type tickMsg int

func tick(id int) tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(time.Time) tea.Msg {
		return tickMsg(id)
	})
}

// ---------- sound ----------

//go:embed sounds/*.mp3
var soundFS embed.FS

const (
	soundBreakDone   = "brr-brr-patapim-alarm-clock.mp3" // end of every break
	soundSessionDone = "outro-songg.mp3"                 // end of the whole session
)

// One per phase, played when that phase's work block finishes.
// Reorder freely. If there are fewer sounds than phases, they wrap around.
var phaseDoneSounds = []string{
	"bad-to-the-bone-meme.mp3",
	"kids-saying-yay-sound-effect_3.mp3",
	"heavenly-music-gaming-sound-effect-hd-mp3cut.mp3",
	"039-a-victory-fanfare.mp3",
	"victory_6.mp3",
	"confetti-pop-sound.mp3",
}

const sampleRate = beep.SampleRate(44100)

var audioReady bool

func initAudio() {
	if err := speaker.Init(sampleRate, sampleRate.N(time.Second/10)); err == nil {
		audioReady = true
	}
}

// playSound decodes an embedded mp3 and plays it. Falls back to the
// terminal bell if audio isn't available.
func playSound(name string) {
	if !audioReady {
		fmt.Print("\a")
		return
	}
	data, err := soundFS.ReadFile("sounds/" + name)
	if err != nil {
		fmt.Print("\a")
		return
	}
	s, format, err := mp3.Decode(io.NopCloser(bytes.NewReader(data)))
	if err != nil {
		fmt.Print("\a")
		return
	}
	resampled := beep.Resample(4, format.SampleRate, sampleRate, s)

	// The break alarm gets a Ctrl wrapper so a key press can cut it off.
	if name == soundBreakDone {
		c := &beep.Ctrl{Streamer: resampled}
		speaker.Lock()
		alarm = c
		speaker.Unlock()
		speaker.Play(beep.Seq(c, beep.Callback(func() {
			// runs under the speaker lock, so no extra locking here
			if alarm == c {
				alarm = nil
			}
			s.Close()
		})))
		return
	}
	speaker.Play(beep.Seq(resampled, beep.Callback(func() { s.Close() })))
}

// alarm is the currently ringing break alarm (nil if none).
// Only touch it while holding the speaker lock.
var alarm *beep.Ctrl

// stopAlarm silences the break alarm. It reports whether one was ringing.
func stopAlarm() bool {
	if !audioReady {
		return false
	}
	speaker.Lock()
	defer speaker.Unlock()
	if alarm == nil {
		return false
	}
	alarm.Streamer = nil // ends the stream; the callback then cleans up
	alarm = nil
	return true
}

// sfx wraps playSound as a tea.Cmd (runs off the UI goroutine).
func sfx(name string) tea.Cmd {
	if name == "" {
		return nil
	}
	return func() tea.Msg {
		playSound(name)
		return nil
	}
}

func newModel() model {
	m := model{}
	m.start(session[0].Work)
	return m
}

func (m *model) start(d time.Duration) {
	m.stageDur = d
	m.remaining = d
	m.end = time.Now().Add(d)
}

// advance moves to the next stage and returns the sound that belongs to
// the transition ("" for none).
func (m *model) advance() string {
	switch m.stage {
	case working:
		snd := phaseDoneSounds[m.phaseIdx%len(phaseDoneSounds)]
		m.stage = onBreak
		m.start(session[m.phaseIdx].Break)
		return snd
	case onBreak:
		m.phaseIdx++
		if m.phaseIdx >= len(session) {
			m.phaseIdx = len(session) - 1
			m.stage = done
			m.stageDur, m.remaining = 0, 0
			return soundSessionDone
		}
		m.stage = working
		m.start(session[m.phaseIdx].Work)
		return soundBreakDone
	}
	return ""
}

func (m model) left() time.Duration {
	switch {
	case m.stage == done:
		return 0
	case m.paused:
		return m.remaining
	}
	if d := time.Until(m.end); d > 0 {
		return d
	}
	return 0
}

func totalSession() time.Duration {
	var t time.Duration
	for _, p := range session {
		t += p.Work + p.Break
	}
	return t
}

func (m model) elapsed() time.Duration {
	if m.stage == done {
		return totalSession()
	}
	var e time.Duration
	for i := 0; i < m.phaseIdx; i++ {
		e += session[i].Work + session[i].Break
	}
	if m.stage == onBreak {
		e += session[m.phaseIdx].Work
	}
	return e + (m.stageDur - m.left())
}

// ---------- bubble tea ----------

func (m model) Init() tea.Cmd { return tick(m.tickID) }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		if int(msg) != m.tickID || m.paused || m.stage == done {
			return m, nil
		}
		if time.Until(m.end) <= 0 {
			snd := m.advance()
			return m, tea.Batch(tick(m.tickID), sfx(snd))
		}
		return m, tick(m.tickID)

	case tea.KeyMsg:
		// While the break alarm is ringing, the first key press only
		// silences it (so a stray key doesn't pause the new work block).
		// Quit keys still quit.
		k := msg.String()
		if stopAlarm() && k != "q" && k != "ctrl+c" {
			return m, nil
		}
		switch k {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit

		case " ", "space", "p":
			if m.stage == done {
				return m, nil
			}
			if !m.paused {
				m.remaining = time.Until(m.end)
				m.paused = true
				return m, nil
			}
			m.end = time.Now().Add(m.remaining)
			m.paused = false
			m.tickID++
			return m, tick(m.tickID)

		case "n", "s":
			if m.stage == done {
				return m, nil
			}
			m.advance() // skipping is silent
			return m, nil

		case "r":
			w, h, id := m.width, m.height, m.tickID+1
			m = newModel()
			m.width, m.height, m.tickID = w, h, id
			return m, tick(id)
		}
	}
	return m, nil
}

// ---------- view helpers ----------

func fmtClock(d time.Duration) string {
	s := int(math.Ceil(d.Seconds()))
	if s < 0 {
		s = 0
	}
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, (s%3600)/60, s%60)
	}
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

var glyphs = map[rune][5]string{
	'0': {"███", "█ █", "█ █", "█ █", "███"},
	'1': {" █ ", "██ ", " █ ", " █ ", "███"},
	'2': {"███", "  █", "███", "█  ", "███"},
	'3': {"███", "  █", "███", "  █", "███"},
	'4': {"█ █", "█ █", "███", "  █", "  █"},
	'5': {"███", "█  ", "███", "  █", "███"},
	'6': {"███", "█  ", "███", "█ █", "███"},
	'7': {"███", "  █", "  █", "  █", "  █"},
	'8': {"███", "█ █", "███", "█ █", "███"},
	'9': {"███", "█ █", "███", "  █", "███"},
	':': {" ", "█", " ", "█", " "},
}

func bigClock(s string, c1, c2 string) string {
	rows := make([]string, 5)
	i := 0
	for _, ch := range s {
		g, ok := glyphs[ch]
		if !ok {
			continue
		}
		col := c1
		if i%2 == 1 {
			col = c2
		}
		i++
		st := fg(col).Bold(true)
		for r := 0; r < 5; r++ {
			rows[r] += st.Render(g[r]) + " "
		}
	}
	return strings.Join(rows, "\n")
}

// ---------- pixel-art banner ----------

var pixFont = map[rune][5]string{
	'A': {" ### ", "#   #", "#####", "#   #", "#   #"},
	'D': {"#### ", "#   #", "#   #", "#   #", "#### "},
	'H': {"#   #", "#   #", "#####", "#   #", "#   #"},
	'O': {" ### ", "#   #", "#   #", "#   #", " ### "},
	'R': {"#### ", "#   #", "#### ", "#  # ", "#   #"},
}

// pixelWord draws a word as chunky pixels, one colour per letter,
// with a 1-pixel drop shadow.
func pixelWord(word string, colors []string, scale int) string {
	rows := 5
	cols := len(word)*6 - 1
	on := make([][]int, rows+1)
	for r := range on {
		on[r] = make([]int, cols+1)
	}
	for li := 0; li < len(word); li++ {
		g := pixFont[rune(word[li])]
		for r := 0; r < rows; r++ {
			for c := 0; c < 5; c++ {
				if g[r][c] == '#' {
					on[r][li*6+c] = li%len(colors) + 1
				}
			}
		}
	}
	var lines []string
	for r := 0; r <= rows; r++ {
		var sb strings.Builder
		for c := 0; c <= cols; c++ {
			switch {
			case on[r][c] != 0:
				sb.WriteString(fg(colors[on[r][c]-1]).Render(strings.Repeat("█", scale)))
			case r > 0 && c > 0 && on[r-1][c-1] != 0:
				sb.WriteString(fg(shadow).Render(strings.Repeat("▒", scale)))
			default:
				sb.WriteString(strings.Repeat(" ", scale))
			}
		}
		lines = append(lines, sb.String())
	}
	return strings.Join(lines, "\n")
}

// harlequin is a row of alternating, multi-coloured diamonds.
func harlequin(n int) string {
	cols := []string{orange, gold, teal, green, violet}
	var sb strings.Builder
	for i := 0; i < n; i++ {
		g := "◆"
		if i%2 == 1 {
			g = "◇"
		}
		sb.WriteString(fg(cols[i%len(cols)]).Render(g))
	}
	return sb.String()
}

var bannerCache = map[int]string{}

// banner is the big ADHD / DORO logo (cached, since it never changes).
func banner(inner int) string {
	if b, ok := bannerCache[inner]; ok {
		return b
	}
	scale := 1
	if inner >= 50 {
		scale = 2
	}
	top := pixelWord("ADHD", []string{orange, green, teal, gold}, scale)
	bot := pixelWord("DORO", []string{violet, orange, teal, green}, scale)
	b := lipgloss.JoinVertical(lipgloss.Center, top, harlequin(lipgloss.Width(top)), bot)
	bannerCache[inner] = b
	return b
}

var jesterBorder = lipgloss.Border{
	Top:         "◆◇",
	Bottom:      "◇◆",
	Left:        "◆",
	Right:       "◇",
	TopLeft:     "✦",
	TopRight:    "✦",
	BottomLeft:  "✦",
	BottomRight: "✦",
}

func bar(a, b string, w int, pct float64) string {
	p := progress.New(
		progress.WithGradient(a, b),
		progress.WithoutPercentage(),
		progress.WithWidth(w),
	)
	p.EmptyColor = track
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	return p.ViewAs(pct)
}

// spread puts left and right text on one line, pushed to opposite edges.
func spread(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func fg(color string) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(color))
}

// taper draws the phases as bars that shrink with the work blocks.
func (m model) taper(accent string, inner int) string {
	heights := []string{"▂", "▃", "▄", "▅", "▆", "▇"}
	n := len(session)

	// shrink the bars if there are many phases
	cellW, sep := 3, " "
	switch {
	case n*4-1 <= inner:
	case n*2-1 <= inner:
		cellW = 1
	default:
		cellW, sep = 1, ""
	}
	blank := strings.Repeat(" ", cellW)
	marker := " ▲ "
	if cellW == 1 {
		marker = "▲"
	}

	wmax, wmin := session[0].Work, session[n-1].Work
	var bars, marks []string
	for i := range session {
		t := 1.0
		if wmax != wmin {
			t = float64(session[i].Work-wmin) / float64(wmax-wmin)
		}
		idx := int(t*float64(len(heights)-1) + 0.5)
		cell := strings.Repeat(heights[idx], cellW)
		switch {
		case m.stage == done || i < m.phaseIdx:
			bars = append(bars, fg(green).Render(cell))
			marks = append(marks, blank)
		case i == m.phaseIdx:
			bars = append(bars, fg(accent).Bold(true).Render(cell))
			marks = append(marks, fg(accent).Render(marker))
		default:
			bars = append(bars, fg(track).Render(cell))
			marks = append(marks, blank)
		}
	}
	return strings.Join(bars, sep) + "\n" + strings.Join(marks, sep)
}

func (m model) nextUp() string {
	switch m.stage {
	case working:
		return "up next: break · " + fmtClock(session[m.phaseIdx].Break)
	case onBreak:
		if m.phaseIdx+1 >= len(session) {
			return "up next: session complete"
		}
		return fmt.Sprintf("up next: phase %d · %s focus", m.phaseIdx+1, fmtClock(session[m.phaseIdx+1].Work))
	}
	return ""
}

// ---------- view ----------

func (m model) View() string {
	cardW := 64
	if m.width > 0 && m.width-4 < cardW {
		cardW = m.width - 4
	}
	if cardW < 40 {
		cardW = 40
	}
	inner := cardW - 8 // border (2) + padding (3 each side)

	// colours depend on stage
	accent, accent2, label := orange, gold, "♦ FOCUS ♦"
	switch m.stage {
	case onBreak:
		accent, accent2, label = green, teal, "♣ BREAK ♣"
	case done:
		accent, accent2, label = gold, orange, "★ COMPLETE ★"
	}

	sub := fg(teal).Italic(true).Render("✦LOCK INN✦")

	var parts []string
	parts = append(parts, banner(inner), "", sub, "")

	if m.stage == done {
		parts = append(parts,
			fg(gold).Bold(true).Render("★ YOU ABSOLUTE LEGEND ★"),
			"",
			fg(text).Render(fmt.Sprintf("You finished all %d phases.", len(session))),
			fg(text).Render("Take your long break — you earned it."),
			"",
		)
	} else {
		status := fg(accent).Bold(true).Render(label)
		if m.paused {
			status = fg(yellow).Bold(true).Render("⏸ PAUSED")
		}
		phaseTag := fg(dim).Render(fmt.Sprintf("PHASE %d / %d", m.phaseIdx+1, len(session)))
		parts = append(parts, spread(status, phaseTag, inner), "")

		clockA, clockB := accent, accent2
		if m.paused {
			clockA, clockB = dim, dim
		}
		parts = append(parts, bigClock(fmtClock(m.left()), clockA, clockB), "")

		pct := 1 - float64(m.left())/float64(m.stageDur)
		parts = append(parts,
			spread(fg(dim).Render("THIS BLOCK"), fg(accent).Render(fmt.Sprintf("%3.0f%%", pct*100)), inner),
			bar(accent, accent2, inner, pct),
			"",
		)
	}

	sPct := float64(m.elapsed()) / float64(totalSession())
	parts = append(parts,
		spread(fg(dim).Render("WHOLE SESSION"),
			fg(blue).Render(fmt.Sprintf("%s / %s", fmtClock(m.elapsed()), fmtClock(totalSession()))), inner),
		bar(violet, teal, inner, sPct),
		"",
		m.taper(accent, inner),
	)

	if m.stage != done {
		parts = append(parts, "", fg(dim).Render(m.nextUp()))
	}

	card := lipgloss.NewStyle().
		Border(jesterBorder).
		BorderForeground(lipgloss.Color(accent)).
		Padding(1, 3).
		Width(cardW).
		Render(lipgloss.JoinVertical(lipgloss.Center, parts...))

	keys := []string{"space pause", "n skip", "r restart", "q quit"}
	if m.stage == done {
		keys = []string{"r restart", "q quit"}
	}
	help := fg(dim).Render(strings.Join(keys, "  ·  "))

	out := lipgloss.JoinVertical(lipgloss.Center, card, "", help)
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, out)
	}
	return out
}

func main() {
	start := flag.Float64("start", 60, "length of the first work block, in minutes")
	step := flag.Float64("step", 10, "how much each work block shrinks by, in minutes")
	brk := flag.Float64("break", 10, "length of every break, in minutes")
	flag.Parse()

	if *start <= 0 || *step <= 0 || *brk <= 0 {
		fmt.Fprintln(os.Stderr, "start, step and break must all be greater than 0")
		os.Exit(2)
	}
	if *start / *step > maxPhases {
		fmt.Fprintf(os.Stderr, "too many phases (max %d): raise -step or lower -start\n", maxPhases)
		os.Exit(2)
	}
	session = buildSession(*start, *step, *brk)

	initAudio()
	p := tea.NewProgram(newModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}
