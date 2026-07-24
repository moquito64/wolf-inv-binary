package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// --- CONFIGURATION ---

// Config holds application configuration loaded from a JSON file.
type Config struct {
	ApiBaseURL string `json:"apiBaseURL"`
	ApiToken   string `json:"apiToken"` // Added field for the Bearer token
}

// loadConfig reads the configuration from a standard location (~/.config/wolf-inv/config.json).
func loadConfig() (*Config, error) {
	// Get the user's home directory to find the config folder.
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("could not find user home directory: %w", err)
	}

	// Construct the path to the configuration directory.
	configDir := filepath.Join(homeDir, ".config", "wolf-inv")
	configPath := filepath.Join(configDir, "config.json")

	// Create the configuration directory if it doesn't exist.
	if _, err := os.Stat(configDir); os.IsNotExist(err) {
		if err := os.MkdirAll(configDir, 0755); err != nil {
			return nil, fmt.Errorf("could not create config directory at %s: %w", configDir, err)
		}
	}

	// Open the config file from the standard path.
	file, err := os.Open(configPath)
	if err != nil {
		// Provide a helpful error message guiding the user.
		return nil, fmt.Errorf("could not open config file. Please create one at '%s': %w", configPath, err)
	}
	defer file.Close()

	bytes, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("could not read config file: %w", err)
	}

	var config Config
	if err := json.Unmarshal(bytes, &config); err != nil {
		return nil, fmt.Errorf("could not parse config.json: %w", err)
	}

	return &config, nil
}

// httpClient is used for all API calls so a hung server can't block the UI forever.
var httpClient = &http.Client{Timeout: 10 * time.Second}

const pollInterval = 30 * time.Second

// --- MODEL ---

// Server represents a single server entry from the API.
type Server struct {
	Name       string `json:"name"`
	IP         string `json:"ip"`
	Location   string `json:"location"`
	Status     string `json:"status"`
	LastReport string `json:"last_report"`
}

// State represents the current mode of the TUI application.
type State int

const (
	Viewing State = iota
	Adding
	Editing
	Deleting
	Help // New state for the help view
)

// AddingState represents the sub-state when adding/editing a server.
type AddingState int

const (
	InputName AddingState = iota
	InputIP
	InputLocation
	InputStatus
	Confirm
)

// statusItem is a simple item for the list.
type statusItem string

func (i statusItem) FilterValue() string { return string(i) }

// itemDelegate is the list delegate for rendering status options.
type itemDelegate struct{}

func (d itemDelegate) Height() int                                                 { return 1 }
func (d itemDelegate) Spacing() int                                                { return 0 }
func (d itemDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd                   { return nil }
func (d itemDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	s, ok := item.(statusItem)
	if !ok {
		return
	}
	str := string(s)
	if index == m.Index() {
		fmt.Fprintf(w, "> %s", lipgloss.NewStyle().Foreground(lipgloss.Color("#5696E3")).Render(str))
	} else {
		fmt.Fprintf(w, "  %s", str)
	}
}

// Model represents the state of our TUI application.
type model struct {
	servers       []Server
	err           error
	loading       bool
	message       string
	state         State
	addingState   AddingState
	table         table.Model
	textInput     textinput.Model
	statusList    list.Model
	spinner       spinner.Model
	width         int
	height        int
	currentServer Server
	deleteTarget  string
	apiBaseURL    string
	apiToken      string // Added field to store the API token
	// Styles
	headerStyle     lipgloss.Style
	onlineStyle     lipgloss.Style
	offlineStyle    lipgloss.Style
	otherStyle      lipgloss.Style
	tableStyle      lipgloss.Style
	messageStyle    lipgloss.Style
	successStyle    lipgloss.Style
	cancelStyle     lipgloss.Style
	helpStyle       lipgloss.Style
	helpKeyStyle    lipgloss.Style
	helpDescStyle   lipgloss.Style
	formStyle       lipgloss.Style
	dangerStyle     lipgloss.Style
	currentMsgStyle lipgloss.Style
	messageTimer    *time.Timer
}

// Init runs any initial commands for the app.
func (m model) Init() tea.Cmd {
	// Pass the API token to the initial fetch command
	return tea.Batch(fetchServers(m.apiBaseURL, m.apiToken), pollForUpdates(pollInterval), m.spinner.Tick)
}

// --- UPDATE ---

// Update handles user input and messages.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	// Global handling for window size changes: refit the table columns and
	// widgets to the new terminal dimensions.
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.width, m.height = size.Width, size.Height
		m.updateTable()
		m.table.SetHeight(max(size.Height-10, 3))
		m.statusList.SetSize(min(size.Width-8, 40), 8)
		return m, nil
	}

	// Animate the spinner only while something is loading; each loading=true
	// site restarts the tick loop with m.spinner.Tick.
	if tick, ok := msg.(spinner.TickMsg); ok {
		if m.loading {
			m.spinner, cmd = m.spinner.Update(tick)
			return m, cmd
		}
		return m, nil
	}

	// Global handling for the poll tick so the 30s refresh keeps running
	// regardless of which state the tick lands in. tea.Tick fires only once,
	// so it must be re-armed here every time.
	if _, ok := msg.(fetchServersMsg); ok {
		if m.state == Viewing {
			return m, tea.Batch(fetchServers(m.apiBaseURL, m.apiToken), pollForUpdates(pollInterval))
		}
		return m, pollForUpdates(pollInterval)
	}

	// Stop any existing message timer if a new key is pressed
	if _, ok := msg.(tea.KeyMsg); ok {
		if m.messageTimer != nil {
			m.messageTimer.Stop()
		}
	}

	switch m.state {
	case Viewing:
		return updateViewing(msg, m)
	case Adding, Editing:
		return updateAddingEditing(msg, m)
	case Deleting:
		return updateDeleting(msg, m)
	case Help:
		return updateHelp(msg, m)
	}

	return m, cmd
}

// updateViewing handles logic for the main table view.
func updateViewing(msg tea.Msg, m model) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "r":
			m.loading = true
			m.message = "Refreshing data..."
			m.currentMsgStyle = m.messageStyle
			// Pass the token when refreshing
			return m, tea.Batch(fetchServers(m.apiBaseURL, m.apiToken), m.spinner.Tick)
		case "a":
			m.state = Adding
			m.table.Blur()
			m.addingState = InputName
			m.currentServer = Server{}
			m.textInput.Placeholder = "Name"
			m.textInput.Focus()
			m.textInput.SetValue("")
			m.message = m.formStepMessage(1)
			m.currentMsgStyle = m.messageStyle
			return m, textinput.Blink
		case "d":
			if len(m.servers) > 0 {
				if cursor := m.table.Cursor(); cursor >= 0 && cursor < len(m.servers) {
					m.deleteTarget = m.servers[cursor].Name
					m.state = Deleting
					m.message = ""
				}
			}
			return m, nil
		case "e":
			if len(m.servers) > 0 {
				if cursor := m.table.Cursor(); cursor >= 0 && cursor < len(m.servers) {
					m.state = Editing
					m.table.Blur()
					m.addingState = InputName
					m.currentServer = m.servers[cursor]
					m.textInput.Placeholder = "Name"
					m.textInput.Focus()
					m.textInput.SetValue(m.currentServer.Name)
					m.message = m.formStepMessage(1)
					m.currentMsgStyle = m.messageStyle
					return m, textinput.Blink
				}
			}
		case "?":
			m.state = Help
			return m, nil
		}
	case serverMsg:
		m.loading = false
		m.err = nil
		m.servers = msg.servers
		m.updateTable()
		m.message = fmt.Sprintf("Inventory refreshed at %s", time.Now().Format("15:04:05"))
		m.setTempMessage(m.successStyle, m.message)
	case errMsg:
		m.loading = false
		m.err = msg
		m.message = m.err.Error()
		m.currentMsgStyle = m.cancelStyle // Use cancel style for errors
	case clearMessage:
		m.currentMsgStyle = m.messageStyle
	}
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

// updateAddingEditing handles logic for the add/edit forms.
func updateAddingEditing(msg tea.Msg, m model) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	if keyMsg, ok := msg.(tea.KeyMsg); ok && keyMsg.String() == "esc" {
		m.state = Viewing
		m.textInput.Blur()
		m.table.Focus()
		m.setTempMessage(m.cancelStyle, "Cancelled.")
		return m, nil
	}

	switch m.addingState {
	case InputName, InputIP, InputLocation:
		m.textInput, cmd = m.textInput.Update(msg)
		if keyMsg, ok := msg.(tea.KeyMsg); ok && keyMsg.String() == "enter" {
			value := strings.TrimSpace(m.textInput.Value())
			switch m.addingState {
			case InputName:
				if value == "" {
					m.message = m.formStepMessage(1) + " (name is required)"
					return m, nil
				}
				m.currentServer.Name = value
				m.addingState = InputIP
				m.textInput.Placeholder = "IP Address"
				m.textInput.SetValue(m.currentServer.IP)
				m.message = m.formStepMessage(2)
			case InputIP:
				if value == "" {
					m.message = m.formStepMessage(2) + " (IP is required)"
					return m, nil
				}
				m.currentServer.IP = value
				m.addingState = InputLocation
				m.textInput.Placeholder = "Location"
				m.textInput.SetValue(m.currentServer.Location)
				m.message = m.formStepMessage(3)
			case InputLocation:
				m.currentServer.Location = value
				m.addingState = InputStatus
				m.textInput.Blur()
				m.message = m.formStepMessage(4)
			}
			return m, textinput.Blink
		}
	case InputStatus:
		m.statusList, cmd = m.statusList.Update(msg)
		if keyMsg, ok := msg.(tea.KeyMsg); ok && keyMsg.String() == "enter" {
			selectedStatus := m.statusList.SelectedItem().(statusItem)
			m.currentServer.Status = string(selectedStatus)
			m.addingState = Confirm
			m.message = "" // Clear message for the combined confirmation view
		}
	case Confirm:
		if keyMsg, ok := msg.(tea.KeyMsg); ok {
			switch keyMsg.String() {
			case "y", "Y":
				m.state = Viewing
				m.loading = true
				m.table.Focus()
				m.setTempMessage(m.successStyle, "Submitting server data...")
				// Pass the token when adding/editing
				return m, tea.Batch(addOrEditServer(m.apiBaseURL, m.apiToken, m.currentServer), m.spinner.Tick)
			case "n", "N", "esc":
				m.state = Viewing
				m.table.Focus()
				m.setTempMessage(m.cancelStyle, "Cancelled.")
			}
		}
	}
	return m, cmd
}

// updateDeleting handles logic for the delete confirmation.
func updateDeleting(msg tea.Msg, m model) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.String() {
		case "y", "Y":
			m.state = Viewing
			m.loading = true
			m.table.Focus()
			m.setTempMessage(m.successStyle, fmt.Sprintf("Deleting server '%s'...", m.deleteTarget))
			// Pass the token when deleting
			return m, tea.Batch(deleteServer(m.apiBaseURL, m.apiToken, m.deleteTarget), m.spinner.Tick)
		case "n", "N", "esc":
			m.state = Viewing
			m.table.Focus()
			m.setTempMessage(m.cancelStyle, "Deletion cancelled.")
		}
	}
	return m, nil
}

// updateHelp handles logic for the help view.
func updateHelp(msg tea.Msg, m model) (tea.Model, tea.Cmd) {
	if _, ok := msg.(tea.KeyMsg); ok {
		m.state = Viewing
	}
	return m, nil
}

// --- VIEW ---

// View renders the TUI to the terminal.
func (m model) View() string {
	if m.state == Help {
		return m.helpView()
	}

	s := ""
	header := m.headerStyle.Render(" Server Inventory ")
	if summary := m.summaryLine(); summary != "" {
		header += "  " + summary
	}
	s += header + "\n\n"

	if m.loading {
		s += m.spinner.View() + m.messageStyle.Render(" Loading...")
	} else {
		s += m.currentMsgStyle.Render(m.message)
	}
	s += "\n\n"

	switch m.state {
	case Viewing:
		s += m.viewingView()
	case Adding, Editing:
		s += m.addingEditingView()
	case Deleting:
		s += m.deletingView()
	}

	return s
}

// summaryLine renders a compact online/offline/other count under the header.
func (m model) summaryLine() string {
	if len(m.servers) == 0 {
		return ""
	}
	online, offline, other := 0, 0, 0
	for _, s := range m.servers {
		switch s.Status {
		case "Online":
			online++
		case "Offline":
			offline++
		default:
			other++
		}
	}
	parts := []string{m.helpDescStyle.Render(fmt.Sprintf("%d servers", len(m.servers)))}
	if online > 0 {
		parts = append(parts, m.onlineStyle.Render(fmt.Sprintf("● %d online", online)))
	}
	if offline > 0 {
		parts = append(parts, m.offlineStyle.Render(fmt.Sprintf("● %d offline", offline)))
	}
	if other > 0 {
		parts = append(parts, m.otherStyle.Render(fmt.Sprintf("● %d other", other)))
	}
	return strings.Join(parts, m.helpDescStyle.Render(" · "))
}

// helpBar renders alternating key/description pairs as a styled hint bar.
func (m model) helpBar(pairs ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			b.WriteString(m.helpDescStyle.Render("  ·  "))
		}
		b.WriteString(m.helpKeyStyle.Render(pairs[i]))
		b.WriteString(m.helpDescStyle.Render(" " + pairs[i+1]))
	}
	return b.String()
}

// deletingView renders the delete confirmation as a warning panel.
func (m model) deletingView() string {
	warning := fmt.Sprintf("Delete server '%s'?\n\nThis cannot be undone.", m.deleteTarget)
	return m.dangerStyle.Render(warning) + "\n\n" + m.helpBar("y", "confirm", "n/esc", "cancel")
}

// viewingView renders the main table.
func (m model) viewingView() string {
	s := ""
	if len(m.servers) > 0 {
		tableView := m.table.View()
		lines := strings.Split(tableView, "\n")
		selectedRowIndex := m.table.Cursor()
		serverIndex := 0

		for i, line := range lines {
			if !strings.Contains(line, "│") || strings.Contains(line, "Name") || strings.Contains(line, "─") {
				continue
			}
			if serverIndex < len(m.servers) {
				server := m.servers[serverIndex]
				var statusStyle lipgloss.Style
				switch server.Status {
				case "Online":
					statusStyle = m.onlineStyle
				case "Offline":
					statusStyle = m.offlineStyle
				default:
					statusStyle = m.otherStyle
				}
				paddedStatus := paddedStatusCell(server.Status)
				coloredStatus := statusStyle.Render("● " + server.Status)
				line = strings.Replace(line, paddedStatus, coloredStatus, 1)
				if serverIndex%2 == 1 && serverIndex != selectedRowIndex {
					line = lipgloss.NewStyle().Background(lipgloss.Color("236")).Render(line)
				}
				lines[i] = line
				serverIndex++
			}
		}
		s += m.tableStyle.Render(strings.Join(lines, "\n"))
	} else {
		s += "No servers in inventory. Press 'a' to add one."
	}
	s += "\n\n" + m.helpBar("a", "add", "e", "edit", "d", "delete", "r", "refresh", "?", "help", "q", "quit")
	return s
}

// addingEditingView renders the form for adding or editing a server.
func (m model) addingEditingView() string {
	var body, hints string
	switch m.addingState {
	case InputName, InputIP, InputLocation:
		body = fmt.Sprintf("Enter %s\n\n%s", m.textInput.Placeholder, m.textInput.View())
		hints = m.helpBar("enter", "next", "esc", "cancel")
	case InputStatus:
		body = m.statusList.View()
		hints = m.helpBar("↑/↓", "select", "enter", "next", "esc", "cancel")
	case Confirm:
		label := m.helpDescStyle
		body = fmt.Sprintf("Confirm entry?\n\n  %s %s\n  %s %s\n  %s %s\n  %s %s",
			label.Render("Name:    "), m.currentServer.Name,
			label.Render("IP:      "), m.currentServer.IP,
			label.Render("Location:"), m.currentServer.Location,
			label.Render("Status:  "), m.currentServer.Status)
		hints = m.helpBar("y", "submit", "n/esc", "cancel")
	}
	return m.formStyle.Render(body) + "\n\n" + hints
}

// helpView renders the help screen.
func (m model) helpView() string {
	keys := [][2]string{
		{"a", "Add a new server"},
		{"e", "Edit selected server"},
		{"d", "Delete selected server"},
		{"r", "Refresh server list"},
		{"↑/↓", "Move selection"},
		{"?", "Show this help menu"},
		{"q", "Quit the application"},
	}
	var b strings.Builder
	b.WriteString(m.headerStyle.Render(" Help ") + "\n\n")
	for _, k := range keys {
		b.WriteString(fmt.Sprintf("  %s  %s\n", m.helpKeyStyle.Render(fmt.Sprintf("%-4s", k[0])), m.helpDescStyle.Render(k[1])))
	}
	b.WriteString("\n" + m.messageStyle.Render("Press any key to return."))
	return m.helpStyle.Render(b.String())
}

// --- UTILITIES ---

// paddedStatusCell builds the plain status cell text ("● Online" padded to the
// column width). viewingView relies on producing this exact string so it can
// swap it for a colored version in the rendered table output.
func paddedStatusCell(status string) string {
	s := "● " + status
	if w := lipgloss.Width(s); w < 14 {
		s += strings.Repeat(" ", 14-w)
	}
	return s
}

// updateTable updates the table model with new server data and fits the
// columns to the current terminal width.
func (m *model) updateTable() {
	// Horizontal chrome around column content: tableStyle border+padding (4)
	// plus the table's own per-column cell padding (2 x 5 columns).
	avail := m.width - 14
	if m.width == 0 {
		avail = 105 // no WindowSizeMsg seen yet; use the old fixed layout
	}
	avail = min(max(avail, 62), 140)

	// Status and IP are fixed-width; Name, Location, and Last Report share
	// the rest. Status must stay >= 14 so paddedStatusCell isn't truncated,
	// which would break the color-swap in viewingView.
	statusW, ipW := 14, 18
	rest := avail - statusW - ipW
	nameW := rest * 30 / 100
	locW := rest * 25 / 100
	lastW := rest - nameW - locW

	columns := []table.Column{
		{Title: "Name", Width: nameW}, {Title: "IP Address", Width: ipW},
		{Title: "Location", Width: locW}, {Title: "Status", Width: statusW},
		{Title: "Last Report", Width: lastW},
	}
	m.table.SetWidth(avail + 10)
	rows := []table.Row{}
	for _, server := range m.servers {
		rows = append(rows, table.Row{server.Name, server.IP, server.Location, paddedStatusCell(server.Status), server.LastReport})
	}
	m.table.SetColumns(columns)
	m.table.SetRows(rows)
	s := table.DefaultStyles()
	s.Header = s.Header.BorderStyle(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240")).BorderBottom(true).Bold(false)
	s.Selected = s.Selected.Foreground(lipgloss.Color("229")).Background(lipgloss.Color("99")).Bold(false)
	m.table.SetStyles(s)
}

// formStepMessage builds the step banner for the add/edit form based on the current state.
func (m model) formStepMessage(step int) string {
	verb := "Adding new"
	if m.state == Editing {
		verb = "Editing"
	}
	return fmt.Sprintf("%s server (Step %d of 4):", verb, step)
}

// setTempMessage sets a message with a specific style and a timer to reset it.
func (m *model) setTempMessage(style lipgloss.Style, message string) {
	m.message = message
	m.currentMsgStyle = style
	if m.messageTimer != nil {
		m.messageTimer.Stop()
	}
	m.messageTimer = time.AfterFunc(2*time.Second, func() {
		p.Send(clearMessage{})
	})
}

// --- COMMANDS & MESSAGES ---

type serverMsg struct{ servers []Server }
type errMsg struct{ err error }

func (e errMsg) Error() string { return e.err.Error() }

type fetchServersMsg struct{}
type clearMessage struct{}

// Updated fetchServers to accept and use the API token
func fetchServers(apiURL, apiToken string) tea.Cmd {
	return func() tea.Msg {
		req, err := http.NewRequest("GET", apiURL+"/inventory", nil)
		if err != nil {
			return errMsg{err: fmt.Errorf("could not create request: %w", err)}
		}
		// Set the Authorization header
		req.Header.Set("Authorization", "Bearer "+apiToken)

		resp, err := httpClient.Do(req)
		if err != nil {
			return errMsg{err: fmt.Errorf("could not connect to API: %w", err)}
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return errMsg{err: fmt.Errorf("API request failed with status code %d", resp.StatusCode)}
		}
		var servers []Server
		if err := json.NewDecoder(resp.Body).Decode(&servers); err != nil {
			return errMsg{err: fmt.Errorf("failed to decode JSON: %w", err)}
		}
		// The API is backed by an unordered store, so sort for a stable table.
		sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
		return serverMsg{servers: servers}
	}
}

// Updated addOrEditServer to accept and use the API token
func addOrEditServer(apiURL, apiToken string, serverData Server) tea.Cmd {
	return func() tea.Msg {
		jsonData, err := json.Marshal(serverData)
		if err != nil {
			return errMsg{err: fmt.Errorf("could not encode server data: %w", err)}
		}
		req, err := http.NewRequest("POST", apiURL+"/report", bytes.NewBuffer(jsonData))
		if err != nil {
			return errMsg{err: fmt.Errorf("could not create request: %w", err)}
		}
		// Set headers
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiToken)

		resp, err := httpClient.Do(req)
		if err != nil {
			return errMsg{err: fmt.Errorf("failed to send request: %w", err)}
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return errMsg{err: fmt.Errorf("API request failed: %s", string(body))}
		}
		// Pass the token to the subsequent fetch
		return fetchServers(apiURL, apiToken)()
	}
}

// Updated deleteServer to accept and use the API token
func deleteServer(apiURL, apiToken, serverName string) tea.Cmd {
	return func() tea.Msg {
		// Escape the name so servers named with spaces, slashes, etc. still delete correctly.
		req, err := http.NewRequest("DELETE", fmt.Sprintf("%s/delete/%s", apiURL, url.PathEscape(serverName)), nil)
		if err != nil {
			return errMsg{err: fmt.Errorf("could not create request: %w", err)}
		}
		// Set the Authorization header
		req.Header.Set("Authorization", "Bearer "+apiToken)

		resp, err := httpClient.Do(req)
		if err != nil {
			return errMsg{err: fmt.Errorf("failed to send request: %w", err)}
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return errMsg{err: fmt.Errorf("API request failed: %s", string(body))}
		}
		// Pass the token to the subsequent fetch
		return fetchServers(apiURL, apiToken)()
	}
}

func pollForUpdates(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return fetchServersMsg{}
	})
}

// newFormInput builds the shared form text input with a stable width so the
// form panel doesn't collapse around short placeholder text.
func newFormInput() textinput.Model {
	ti := textinput.New()
	ti.Width = 36
	ti.CharLimit = 64
	return ti
}

// --- MAIN ---

var p *tea.Program

func main() {
	config, err := loadConfig()
	if err != nil {
		fmt.Printf("Error loading configuration: %v\n", err)
		os.Exit(1)
	}

	items := []list.Item{statusItem("Online"), statusItem("Offline"), statusItem("Maintenance")}

	// Initialize styles
	messageStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("7")).Italic(true)
	accent := lipgloss.Color("99")

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(accent)

	m := model{
		apiBaseURL:      config.ApiBaseURL,
		apiToken:        config.ApiToken, // Store the token in the model
		loading:         true,
		message:         "Initializing...",
		state:           Viewing,
		table:           table.New(),
		textInput:       newFormInput(),
		statusList:      list.New(items, itemDelegate{}, 0, 0),
		spinner:         sp,
		headerStyle:     lipgloss.NewStyle().Foreground(lipgloss.Color("229")).Background(accent).Bold(true),
		onlineStyle:     lipgloss.NewStyle().Foreground(lipgloss.Color("10")),
		offlineStyle:    lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
		otherStyle:      lipgloss.NewStyle().Foreground(lipgloss.Color("11")),
		tableStyle:      lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("6")).Padding(1),
		messageStyle:    messageStyle,
		successStyle:    messageStyle.Foreground(lipgloss.Color("10")), // Green
		cancelStyle:     messageStyle.Foreground(lipgloss.Color("11")), // Yellow
		helpStyle:       lipgloss.NewStyle().Padding(1, 2).Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("6")),
		helpKeyStyle:    lipgloss.NewStyle().Foreground(accent).Bold(true),
		helpDescStyle:   lipgloss.NewStyle().Foreground(lipgloss.Color("241")),
		formStyle:       lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(1, 2).Width(46),
		dangerStyle:     lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("9")).Foreground(lipgloss.Color("9")).Padding(1, 2),
		currentMsgStyle: messageStyle,
	}
	m.statusList.Title = "Select Server Status"
	m.updateTable()
	m.table.Focus()

	p = tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Printf("An error occurred: %v\n", err)
		os.Exit(1)
	}
}

