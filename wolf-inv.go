package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
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

// loadConfig reads the configuration from config.json.
func loadConfig() (*Config, error) {
	file, err := os.Open("config.json")
	if err != nil {
		return nil, fmt.Errorf("could not open config.json: %w. Please create one", err)
	}
	defer file.Close()

	bytes, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("could not read config.json: %w", err)
	}

	var config Config
	if err := json.Unmarshal(bytes, &config); err != nil {
		return nil, fmt.Errorf("could not parse config.json: %w", err)
	}

	return &config, nil
}

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

func (d itemDelegate) Height() int                                                    { return 1 }
func (d itemDelegate) Spacing() int                                                   { return 0 }
func (d itemDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd                      { return nil }
func (d itemDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	s, ok := item.(statusItem)
	if !ok {
		return
	}
	str := fmt.Sprintf("%s", s)
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
	currentServer Server
	deleteTarget  string
	apiBaseURL    string
	apiToken      string // Added field to store the API token
	// Styles
	spinnerStyle    lipgloss.Style
	headerStyle     lipgloss.Style
	onlineStyle     lipgloss.Style
	offlineStyle    lipgloss.Style
	otherStyle      lipgloss.Style
	tableStyle      lipgloss.Style
	messageStyle    lipgloss.Style
	successStyle    lipgloss.Style
	cancelStyle     lipgloss.Style
	helpStyle       lipgloss.Style
	currentMsgStyle lipgloss.Style
	messageTimer    *time.Timer
}

// Init runs any initial commands for the app.
func (m model) Init() tea.Cmd {
	// Pass the API token to the initial fetch command
	return tea.Batch(fetchServers(m.apiBaseURL, m.apiToken), pollForUpdates(30*time.Second))
}

// --- UPDATE ---

// Update handles user input and messages.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	// Global handling for window size changes
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.table, cmd = m.table.Update(size)
		m.statusList, _ = m.statusList.Update(size)
		return m, cmd
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
			return m, fetchServers(m.apiBaseURL, m.apiToken)
		case "a":
			m.state = Adding
			m.table.Blur()
			m.addingState = InputName
			m.currentServer = Server{}
			m.textInput.Placeholder = "Name"
			m.textInput.Focus()
			m.textInput.SetValue("")
			m.message = "Adding new server (Step 1 of 4):"
			m.currentMsgStyle = m.messageStyle
			return m, textinput.Blink
		case "d":
			if len(m.servers) > 0 {
				selectedRow := m.table.SelectedRow()
				if len(selectedRow) > 0 {
					m.deleteTarget = selectedRow[0]
					m.state = Deleting
					m.message = ""
				}
			}
			return m, nil
		case "e":
			if len(m.servers) > 0 {
				selectedRow := m.table.SelectedRow()
				if len(selectedRow) > 0 {
					m.state = Editing
					m.table.Blur()
					m.addingState = InputName
					m.currentServer = Server{
						Name:       selectedRow[0],
						IP:         selectedRow[1],
						Location:   selectedRow[2],
						Status:     strings.TrimSpace(selectedRow[3]),
						LastReport: selectedRow[4],
					}
					m.textInput.Placeholder = "Name"
					m.textInput.Focus()
					m.textInput.SetValue(m.currentServer.Name)
					m.message = "Editing server (Step 1 of 4):"
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
	case fetchServersMsg:
		// Pass the token for polling updates
		return m, fetchServers(m.apiBaseURL, m.apiToken)
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
			switch m.addingState {
			case InputName:
				m.currentServer.Name = m.textInput.Value()
				m.addingState = InputIP
				m.textInput.Placeholder = "IP Address"
				m.textInput.SetValue(m.currentServer.IP)
				m.message = "Adding new server (Step 2 of 4):"
			case InputIP:
				m.currentServer.IP = m.textInput.Value()
				m.addingState = InputLocation
				m.textInput.Placeholder = "Location"
				m.textInput.SetValue(m.currentServer.Location)
				m.message = "Adding new server (Step 3 of 4):"
			case InputLocation:
				m.currentServer.Location = m.textInput.Value()
				m.addingState = InputStatus
				m.textInput.Blur()
				m.message = "Adding new server (Step 4 of 4):"
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
				return m, addOrEditServer(m.apiBaseURL, m.apiToken, m.currentServer)
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
			return m, deleteServer(m.apiBaseURL, m.apiToken, m.deleteTarget)
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
	s += m.headerStyle.Render("Server Inventory Dashboard") + "\n\n"

	if m.loading {
		s += m.spinnerStyle.Render("⠋") + " Loading..."
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
		s += fmt.Sprintf("Are you sure you want to delete '%s'?\n\n", m.deleteTarget) + m.messageStyle.Render("Press 'y' to confirm, 'n' or 'Esc' to cancel.")
	}

	return s
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
				paddedStatus := server.Status
				if len(paddedStatus) < 12 {
					paddedStatus = paddedStatus + strings.Repeat(" ", 12-len(paddedStatus))
				}
				coloredStatus := statusStyle.Render(server.Status)
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
	s += "\n\n" + m.messageStyle.Render("'a' add | 'd' delete | 'e' edit | '?' help | 'q' quit")
	return s
}

// addingEditingView renders the form for adding or editing a server.
func (m model) addingEditingView() string {
	s := ""
	switch m.addingState {
	case InputName, InputIP, InputLocation:
		s += fmt.Sprintf("Enter %s:\n\n%s", m.textInput.Placeholder, m.textInput.View())
		s += "\n\n" + m.messageStyle.Render("Press 'Enter' to confirm, 'Esc' to cancel.")
	case InputStatus:
		s += fmt.Sprintf("Select a Status:\n\n%s", m.statusList.View())
		s += "\n\n" + m.messageStyle.Render("Press 'Enter' to confirm, 'Esc' to cancel.")
	case Confirm:
		s += fmt.Sprintf("Confirm entry?\n\n  Name:     %s\n  IP:       %s\n  Location: %s\n  Status:   %s",
			m.currentServer.Name, m.currentServer.IP, m.currentServer.Location, m.currentServer.Status)
		s += "\n\n" + m.messageStyle.Render("Press 'y' to submit, 'n' or 'Esc' to cancel.")
	}
	return s
}

// helpView renders the help screen.
func (m model) helpView() string {
	return m.helpStyle.Render(
		"--- Help ---\n\n"+
			"  a: Add a new server\n"+
			"  e: Edit selected server\n"+
			"  d: Delete selected server\n"+
			"  r: Refresh server list\n"+
			"  ?: Show this help menu\n"+
			"  q: Quit the application\n\n"+
			"Press any key to return to the main view.",
	)
}

// --- UTILITIES ---

// updateTable updates the table model with new server data.
func (m *model) updateTable() {
	columns := []table.Column{
		{Title: "Name", Width: 20}, {Title: "IP Address", Width: 18},
		{Title: "Location", Width: 18}, {Title: "Status", Width: 12},
		{Title: "Last Report", Width: 35},
	}
	rows := []table.Row{}
	for _, server := range m.servers {
		status := server.Status
		if len(status) < 12 {
			status = status + strings.Repeat(" ", 12-len(status))
		}
		rows = append(rows, table.Row{server.Name, server.IP, server.Location, status, server.LastReport})
	}
	m.table.SetColumns(columns)
	m.table.SetRows(rows)
	s := table.DefaultStyles()
	s.Header = s.Header.BorderStyle(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240")).BorderBottom(true).Bold(false)
	s.Selected = s.Selected.Foreground(lipgloss.Color("229")).Background(lipgloss.Color("99")).Bold(false)
	m.table.SetStyles(s)
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

		resp, err := http.DefaultClient.Do(req)
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
		return serverMsg{servers: servers}
	}
}

// Updated addOrEditServer to accept and use the API token
func addOrEditServer(apiURL, apiToken string, serverData Server) tea.Cmd {
	return func() tea.Msg {
		jsonData, _ := json.Marshal(serverData)
		req, err := http.NewRequest("POST", apiURL+"/report", bytes.NewBuffer(jsonData))
		if err != nil {
			return errMsg{err: fmt.Errorf("could not create request: %w", err)}
		}
		// Set headers
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiToken)

		resp, err := http.DefaultClient.Do(req)
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
		req, err := http.NewRequest("DELETE", fmt.Sprintf("%s/delete/%s", apiURL, serverName), nil)
		if err != nil {
			return errMsg{err: fmt.Errorf("could not create request: %w", err)}
		}
		// Set the Authorization header
		req.Header.Set("Authorization", "Bearer "+apiToken)

		resp, err := http.DefaultClient.Do(req)
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

	m := model{
		apiBaseURL:      config.ApiBaseURL,
		apiToken:        config.ApiToken, // Store the token in the model
		loading:         true,
		message:         "Initializing...",
		state:           Viewing,
		table:           table.New(),
		textInput:       textinput.New(),
		statusList:      list.New(items, itemDelegate{}, 0, 0),
		spinnerStyle:    lipgloss.NewStyle().Foreground(lipgloss.Color("12")),
		headerStyle:     lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true).MarginBottom(1),
		onlineStyle:     lipgloss.NewStyle().Foreground(lipgloss.Color("10")),
		offlineStyle:    lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
		otherStyle:      lipgloss.NewStyle().Foreground(lipgloss.Color("11")),
		tableStyle:      lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("6")).Padding(1),
		messageStyle:    messageStyle,
		successStyle:    messageStyle.Copy().Foreground(lipgloss.Color("10")), // Green
		cancelStyle:     messageStyle.Copy().Foreground(lipgloss.Color("11")), // Yellow
		helpStyle:       lipgloss.NewStyle().Padding(1, 2).Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("6")),
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

