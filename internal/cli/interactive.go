package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/smykla-skalski/bilgie/pkg/size"
	"github.com/spf13/cobra"
)

type state int

const (
	stateSelection state = iota
	stateConfirmation
	stateCleaning
	stateDone
)

type providerItem struct {
	provider    provider.Provider
	cleanResult *provider.CleanResult
	cleanErr    error
	name        string
	currentFmt  string
	maxFmt      string
	errMsg      string
	current     int64
	max         int64
	overLimit   bool
	available   bool
}

type model struct {
	progress   progress.Model
	ctx        context.Context
	cfg        *config.Config
	selected   map[int]struct{}
	providers  []providerItem
	spinner    spinner.Model
	totalFreed int64
	cursor     int
	cleanIdx   int
	width      int
	height     int
	state      state
	dryRun     bool
	smartMode  bool
	quitting   bool
}

type scanResultMsg struct {
	item providerItem
	idx  int
}

type cleanResultMsg struct {
	err    error
	result provider.CleanResult
	idx    int
}

// InteractiveCmd launches interactive TUI mode.
var InteractiveCmd = &cobra.Command{
	Use:          "interactive",
	Aliases:      []string{"i"},
	Short:        "Interactive cache management mode",
	RunE:         runInteractive,
	SilenceUsage: true,
}

func init() {
	InteractiveCmd.Flags().Bool("dry-run", false, "Preview without deleting")
	InteractiveCmd.Flags().Bool("full", false, "Use full clean mode instead of smart")
}

func runInteractive(cmd *cobra.Command, _ []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	full, _ := cmd.Flags().GetBool("full")
	return RunInteractiveWithLoader(config.NewLoader(), dryRun, !full)
}

// RunInteractiveWithLoader launches interactive mode with specified loader.
func RunInteractiveWithLoader(loader *config.Loader, dryRun, smart bool) error {
	cfg, err := loader.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	providers := cfg.EnabledProviders()
	if len(providers) == 0 {
		fmt.Println("No enabled providers")
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m := newModel(cfg, providers, dryRun, smart, ctx)
	p := tea.NewProgram(m)

	go func() {
		<-ctx.Done()
		p.Send(tea.Quit())
	}()

	if _, err := p.Run(); err != nil {
		return fmt.Errorf("run interactive: %w", err)
	}

	return nil
}

func newModel(cfg *config.Config, providerNames []string, dryRun, smart bool, ctx context.Context) model {
	items := make([]providerItem, len(providerNames))
	for i, name := range providerNames {
		items[i] = providerItem{name: name}
	}

	s := spinner.New(
		spinner.WithSpinner(spinner.Dot),
		spinner.WithStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("205"))),
	)

	prog := progress.New(progress.WithDefaultBlend())

	if ctx == nil {
		ctx = context.Background()
	}

	return model{
		state:     stateSelection,
		providers: items,
		selected:  make(map[int]struct{}),
		spinner:   s,
		progress:  prog,
		cfg:       cfg,
		ctx:       ctx,
		dryRun:    dryRun,
		smartMode: smart,
		width:     80,
		height:    24,
	}
}

func (m model) Init() tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(m.providers)+1)
	for i := range m.providers {
		cmds = append(cmds, m.scanProviderCmd(i))
	}
	cmds = append(cmds, m.spinner.Tick)
	return tea.Batch(cmds...)
}

func (m model) scanProviderCmd(idx int) tea.Cmd {
	return func() tea.Msg {
		name := m.providers[idx].name
		item := providerItem{name: name}

		p, err := provider.LoadProvider(name, m.cfg)
		if err != nil {
			item.errMsg = fmt.Sprintf("load error: %v", err)
			return scanResultMsg{idx: idx, item: item}
		}

		item.provider = p
		item.available = p.Available()

		if !item.available {
			item.errMsg = "unavailable"
			return scanResultMsg{idx: idx, item: item}
		}

		currentSize, err := p.CurrentSize(m.ctx)
		if err != nil {
			item.errMsg = fmt.Sprintf("scan error: %v", err)
			return scanResultMsg{idx: idx, item: item}
		}

		item.current = currentSize
		item.currentFmt = size.FormatSize(currentSize)
		item.max = p.MaxSize()
		item.maxFmt = size.FormatSize(p.MaxSize())
		item.overLimit = currentSize > p.MaxSize()

		return scanResultMsg{idx: idx, item: item}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		progressWidth := max(msg.Width-10, 1)
		m.progress.SetWidth(progressWidth)
		return m, nil

	case scanResultMsg:
		m.providers[msg.idx] = msg.item
		return m, nil

	case cleanResultMsg:
		m.providers[msg.idx].cleanResult = &msg.result
		m.providers[msg.idx].cleanErr = msg.err
		if msg.err == nil {
			m.totalFreed += msg.result.BytesCleaned
		}
		return m.cleanNext()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case progress.FrameMsg:
		var cmd tea.Cmd
		m.progress, cmd = m.progress.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.state {
	case stateSelection:
		return m.handleSelectionKey(msg)
	case stateConfirmation:
		return m.handleConfirmationKey(msg)
	case stateDone:
		return m.handleDoneKey(msg)
	case stateCleaning:
		// Key input ignored while cleaning is in progress.
	}
	return m, nil
}

func (m model) handleSelectionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "ctrl+c":
		m.quitting = true
		return m, tea.Quit

	case "j", "down":
		if m.cursor < len(m.providers)-1 {
			m.cursor++
		}

	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}

	case "space":
		if m.providers[m.cursor].available {
			if _, ok := m.selected[m.cursor]; ok {
				delete(m.selected, m.cursor)
			} else {
				m.selected[m.cursor] = struct{}{}
			}
		}

	case "a":
		for i := range m.providers {
			if m.providers[i].available {
				m.selected[i] = struct{}{}
			}
		}

	case "n":
		m.selected = make(map[int]struct{})

	case "o":
		m.selected = make(map[int]struct{})
		for i := range m.providers {
			if m.providers[i].available && m.providers[i].overLimit {
				m.selected[i] = struct{}{}
			}
		}

	case "enter":
		if len(m.selected) > 0 {
			m.state = stateConfirmation
		}
	}

	return m, nil
}

func (m model) handleConfirmationKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		m.state = stateCleaning
		m.cleanIdx = -1
		return m.cleanNext()

	case "n", "N", "esc", "ctrl+c":
		m.state = stateSelection

	case "f", "F":
		m.smartMode = false

	case "s", "S":
		m.smartMode = true
	}

	return m, nil
}

func (m model) handleDoneKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter", "q", "esc", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	}
	return m, nil
}

func (m model) cleanNext() (tea.Model, tea.Cmd) {
	for {
		m.cleanIdx++
		if m.cleanIdx >= len(m.providers) {
			m.state = stateDone
			return m, nil
		}
		if _, ok := m.selected[m.cleanIdx]; ok {
			break
		}
	}

	return m, m.cleanProviderCmd(m.cleanIdx)
}

func (m model) cleanProviderCmd(idx int) tea.Cmd {
	return func() tea.Msg {
		p := m.providers[idx].provider
		if p == nil {
			return cleanResultMsg{idx: idx, err: fmt.Errorf("provider not loaded")}
		}

		mode := provider.CleanModeFull
		if m.smartMode {
			mode = provider.CleanModeSmart
		}

		result, err := p.Clean(m.ctx, provider.CleanOptions{
			DryRun: m.dryRun,
			Mode:   mode,
		})

		return cleanResultMsg{idx: idx, result: result, err: err}
	}
}

func (m model) View() tea.View {
	if m.quitting {
		v := tea.NewView("")
		v.AltScreen = true
		return v
	}

	var b strings.Builder

	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Render("Bilgie - Interactive Mode")
	b.WriteString(title)
	b.WriteString("\n\n")

	switch m.state {
	case stateSelection, stateConfirmation:
		b.WriteString(m.viewSelection())
	case stateCleaning:
		b.WriteString(m.viewCleaning())
	case stateDone:
		b.WriteString(m.viewDone())
	}

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("205")).
		Padding(1, 2).
		Width(m.width - 2)

	content := boxStyle.Render(b.String())

	if m.state == stateConfirmation {
		popup := m.viewConfirmationPopup()
		popupWidth := lipgloss.Width(popup)
		popupHeight := lipgloss.Height(popup)

		bgLines := strings.Split(content, "\n")
		popupLines := strings.Split(popup, "\n")

		startY := (len(bgLines) - popupHeight) / 2
		startX := max((m.width-popupWidth)/2, 0)
		if startY < 0 {
			startY = 0
		}

		for i, pLine := range popupLines {
			bgIdx := startY + i
			if bgIdx >= 0 && bgIdx < len(bgLines) {
				bgLine := bgLines[bgIdx]
				bgWidth := lipgloss.Width(bgLine)

				var result strings.Builder
				if startX > 0 {
					result.WriteString(truncateRight(bgLine, startX))
				}
				result.WriteString(pLine)
				endX := startX + popupWidth
				if endX < bgWidth {
					suffix := truncateLeft(bgLine, endX)
					result.WriteString(suffix)
				}
				bgLines[bgIdx] = result.String()
			}
		}
		content = strings.Join(bgLines, "\n")
	}

	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

func (m model) viewSelection() string {
	var b strings.Builder

	b.WriteString("Select providers to clean:\n\n")

	// Calculate column widths based on terminal width
	// Layout: cursor(2) + checkbox(4) + name + gap + size + gap + status
	contentWidth := max(
		// box border + padding
		m.width-8, 40)

	// Find max provider name length
	maxNameLen := 0
	for i := range m.providers {
		if len(m.providers[i].name) > maxNameLen {
			maxNameLen = len(m.providers[i].name)
		}
	}
	if maxNameLen < 10 {
		maxNameLen = 10
	}

	// Column widths: prefix(6) + name + size(24) + status(6)
	fixedWidth := 6 + 24 + 6
	nameWidth := max(contentWidth-fixedWidth, maxNameLen)
	const maxNameWidth = 50
	if nameWidth > maxNameWidth {
		nameWidth = maxNameWidth
	}

	for i := range m.providers {
		p := &m.providers[i]
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}

		checkbox := "[ ]"
		if _, ok := m.selected[i]; ok {
			checkbox = "[x]"
		}

		prefix := cursor + checkbox + " "
		nameFmt := fmt.Sprintf("%%-%ds", nameWidth)

		var line string
		switch {
		case p.errMsg != "":
			line = prefix + fmt.Sprintf(nameFmt, p.name) + " " + errorStyle.Render(p.errMsg)
		case p.currentFmt == "":
			sizeCol := fmt.Sprintf("%10s / %-10s", "-", "-")
			line = prefix + fmt.Sprintf(nameFmt, p.name) + " " + sizeCol + " " + m.spinner.View()
		case !p.available:
			line = prefix + dimStyle.Render(fmt.Sprintf(nameFmt+" (unavailable)", p.name))
		default:
			status := okStyle.Render("ok")
			if p.overLimit {
				status = overStyle.Render("OVER")
			}
			sizeCol := fmt.Sprintf("%10s / %-10s", p.currentFmt, p.maxFmt)
			line = prefix + fmt.Sprintf(nameFmt, p.name) + " " + sizeCol + " " + status
		}

		if i == m.cursor {
			line = selectedLineStyle.Render(line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	fmt.Fprintf(&b, "Selected: %d provider(s)", len(m.selected))
	b.WriteString("\n\n")

	hint := dimStyle.Render("space=toggle  a=all  n=none  o=over-limit  enter=confirm  q=quit")
	b.WriteString(hint)

	return b.String()
}

func (m model) viewConfirmationPopup() string {
	var b strings.Builder

	names := m.selectedNames()
	mode := "full"
	if m.smartMode {
		mode = "smart"
	}
	if m.dryRun {
		mode += " (dry-run)"
	}

	fmt.Fprintf(&b, "Clean %d provider(s) [%s]?\n\n", len(names), mode)

	for _, name := range names {
		fmt.Fprintf(&b, "  • %s\n", name)
	}

	b.WriteString("\n")
	hint := dimStyle.Render("y=confirm  n/esc=back  s=smart  f=full")
	b.WriteString(hint)

	popupStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("205")).
		Padding(1, 2)

	return popupStyle.Render(b.String())
}

func (m model) viewCleaning() string {
	var b strings.Builder

	if m.cleanIdx >= 0 && m.cleanIdx < len(m.providers) {
		p := m.providers[m.cleanIdx]
		fmt.Fprintf(&b, "Cleaning %s... %s\n\n", p.name, m.spinner.View())
	}

	selectedCount := len(m.selected)
	completed := 0
	for i := range m.providers {
		if _, ok := m.selected[i]; ok && m.providers[i].cleanResult != nil {
			completed++
		}
	}

	if completed > 0 {
		pct := float64(completed) / float64(selectedCount)
		b.WriteString(m.progress.ViewAs(pct))
		b.WriteString("\n\n")
	}

	if completed > 0 {
		b.WriteString("Completed:\n")
		for i := range m.providers {
			p := &m.providers[i]
			if _, ok := m.selected[i]; !ok {
				continue
			}
			if p.cleanResult != nil {
				if p.cleanErr != nil {
					fmt.Fprintf(&b, "  %-14s %s\n", p.name, errorStyle.Render("error"))
				} else {
					freed := size.FormatSize(p.cleanResult.BytesCleaned)
					fmt.Fprintf(&b, "  %-14s freed %10s\n", p.name, freed)
				}
			}
		}
	}

	return b.String()
}

func (m model) viewDone() string {
	var b strings.Builder

	if m.dryRun {
		b.WriteString(totalStyle.Render(fmt.Sprintf("[dry-run] Would clean %d provider(s)", len(m.selected))))
	} else {
		b.WriteString(totalStyle.Render(fmt.Sprintf("Cleaned %d provider(s), freed %s", len(m.selected), size.FormatSize(m.totalFreed))))
	}
	b.WriteString("\n\n")

	var errors []string
	for i := range m.providers {
		p := &m.providers[i]
		if _, ok := m.selected[i]; !ok {
			continue
		}
		if p.cleanResult != nil {
			if p.cleanErr != nil {
				errors = append(errors, fmt.Sprintf("%s: %v", p.name, p.cleanErr))
				fmt.Fprintf(&b, "  %s   %s\n", p.name, errorStyle.Render("✗"))
			} else {
				freed := size.FormatSize(p.cleanResult.BytesCleaned)
				fmt.Fprintf(&b, "  %-14s %10s  %s\n", p.name, freed, okStyle.Render("✓"))
			}
		}
	}

	if len(errors) > 0 {
		b.WriteString("\n")
		b.WriteString(errorStyle.Render("Errors:"))
		b.WriteString("\n")
		for _, e := range errors {
			fmt.Fprintf(&b, "  %s\n", e)
		}
	}

	b.WriteString("\n")
	b.WriteString(dimStyle.Render("Press enter to exit"))

	return b.String()
}

func (m model) selectedNames() []string {
	var names []string
	for i := range m.providers {
		if _, ok := m.selected[i]; ok {
			names = append(names, m.providers[i].name)
		}
	}
	return names
}

// truncateLeft/truncateRight handle lipgloss CSI sequences (colors, styles).
// Assumes escape sequences end with a letter (a-z/A-Z).
func truncateLeft(s string, skip int) string {
	var result strings.Builder
	width := 0
	inEscape := false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			if width >= skip {
				result.WriteRune(r)
			}
			continue
		}
		if inEscape {
			if width >= skip {
				result.WriteRune(r)
			}
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		if width >= skip {
			result.WriteRune(r)
		}
		width += runewidth.RuneWidth(r)
	}
	return result.String()
}

func truncateRight(s string, keep int) string {
	var result strings.Builder
	width := 0
	inEscape := false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			result.WriteRune(r)
			continue
		}
		if inEscape {
			result.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		rw := runewidth.RuneWidth(r)
		if width+rw > keep {
			break
		}
		result.WriteRune(r)
		width += rw
	}
	return result.String()
}
