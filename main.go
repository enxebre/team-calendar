package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/calendar/v3"
)

func getClient(ctx context.Context, config *oauth2.Config) *http.Client {
	tokFile := "token.json"
	tok, err := tokenFromFile(tokFile)
	if err != nil {
		tok = getTokenFromWeb(ctx, config)
		saveToken(tokFile, tok)
	}
	return config.Client(ctx, tok)
}

func getTokenFromWeb(ctx context.Context, config *oauth2.Config) *oauth2.Token {
	// Start a local web server to listen for the authorization response
	state := "state-token"
	authURL := config.AuthCodeURL(state, oauth2.AccessTypeOffline)
	log.Printf("Go to the following link in your browser: \n%v\n", authURL)

	codeCh := make(chan string)
	http.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("state") != state {
			http.Error(w, "state did not match", http.StatusBadRequest)
			return
		}
		code := query.Get("code")
		codeCh <- code
		log.Println(w, "Authorization completed, you can close this window.")
	})
	go http.ListenAndServe(":8080", nil)

	// Wait for the authorization code from the web server
	code := <-codeCh

	tok, err := config.Exchange(ctx, code)
	if err != nil {
		log.Fatalf("Unable to retrieve token from web: %v", err)
	}
	return tok
}

func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

func saveToken(path string, token *oauth2.Token) {
	log.Printf("Saving credential file to: %s\n", path)
	f, err := os.Create(path)
	if err != nil {
		log.Fatalf("Unable to create token file: %v", err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}

func createRotationalEvent(srv *calendar.Service, calendarId string, summary string, startDate, memberEndDate time.Time, recurrenceRule, colorID string) {
	event := &calendar.Event{
		Summary: summary,
		Start: &calendar.EventDateTime{
			Date:     startDate.Format(time.DateOnly),
			TimeZone: "UTC",
		},

		End: &calendar.EventDateTime{
			Date:            memberEndDate.Format(time.DateOnly),
			TimeZone:        "UTC",
			ForceSendFields: []string{},
			NullFields:      []string{},
		},
		Recurrence: []string{recurrenceRule},
		ColorId:    colorID,
	}

	event, err := srv.Events.Insert(calendarId, event).Do()
	if err != nil {
		log.Fatalf("Unable to create event. %v\n", err)
	}
	log.Printf("Event created: %s\n", event.HtmlLink)
}

func main() {
	var teamMembers []string
	var startDate string
	var duration int
	var eventName string
	var prompt string

	fullPromt := func(actualPromt string) string {
		return fmt.Sprintf(`
I want to run a golang binary that creates a calendar event for a team rotation.
The binary takes the following flags:
  -t, --team-members: Comma-separated list of team members
  -s, --start-date: Start date for the rotation
  -d, --duration: Duration of each event in weeks, e.g. 3
  -n, --event-name: Name of the event, e.g. SRE Role
When I ask you to create an event I want you to return the binary flags with the values I should use.
E.g if I tell you "Create and event called SRE-ROLE for Cesar and Seth that repeats every three weeks starting the first of july"
You should return:
	  -t Cesar,Seth -s 2024-07-01 -d 3 -n SRE-ROLE

E.g if I tell you "Create and event called Interrupt-catcher for Mulham, Juan and Bryan that repeats every 1 week starting the second of july"
You should return:
	  -t Mulham,Juan,Bryan -s 2024-07-02 -d 1 -n Interrupt-catcher

Make sure to return only strictly necessary flags and values formatted as shown in the examples above.
No additional information or text should be returned.	  

Now, this is the real ask: %s
`, actualPromt)
	}

	cmd := &cobra.Command{
		Use:   "calendar",
		Short: "A command-line calendar tool",
		RunE: func(cmd *cobra.Command, args []string) error {
			teamMembers, _ = cmd.Flags().GetStringSlice("team-members")
			startDate, _ = cmd.Flags().GetString("start-date")
			duration, _ = cmd.Flags().GetInt("duration")
			eventName, _ = cmd.Flags().GetString("event-name")
			prompt, _ = cmd.Flags().GetString("prompt")
			ctx := cmd.Context()

			if prompt != "" {
				// get variables from llm run
				llmOutput, err := exec.CommandContext(ctx, "ollama", "run", "llama3", fullPromt(prompt)).Output()
				if err != nil {
					log.Fatalf("Failed to execute ollama: %v", string(llmOutput))
				}

				// Sanitize llm output.
				output := strings.ReplaceAll(strings.TrimSpace(string(llmOutput)), "\n", "")
				log.Printf("Ollama output is: %v", output)

				// Parse the output from ollama into variables
				var teamMembersFullString string
				n, err := fmt.Sscanf(output, "-t %s -s %s -d %d -n %s", &teamMembersFullString, &startDate, &duration, &eventName)
				if err != nil {
					log.Fatalf("Unable to parse output from ollama %v: %v", n, err)
				}
				teamMembers = strings.Split(teamMembersFullString, ",")
				log.Printf("Variables parsed from llm are: Team members: %v, Start date: %v, Duration: %v, Event name: %v", teamMembers, startDate, duration, eventName)
			}

			startDateParsed, err := time.Parse("2006-01-02", startDate)
			if err != nil {
				log.Fatalf("Unable to parse start date: %v", err)
			}

			createEvent(ctx, teamMembers, startDateParsed, duration, eventName)
			return nil
		},
	}

	// flags.
	cmd.Flags().StringSliceVarP(&teamMembers, "team-members", "t", nil, "Comma-separated list of team members")
	cmd.Flags().StringVarP(&startDate, "start-date", "s", "", "Start date for the rotation")
	cmd.Flags().IntVarP(&duration, "duration", "d", 0, "Duration of each event in weeks, e.g. 3")
	cmd.Flags().StringVarP(&eventName, "event-name", "n", "", "Name of the event, e.g. SRE Role")
	cmd.Flags().StringVarP(&prompt, "prompt", "p", "", "Prompt to use to create an event")

	// validations: either prompt or team-members and the other flags should be provided.
	cmd.MarkFlagsRequiredTogether("team-members", "start-date", "duration", "event-name")
	cmd.MarkFlagsMutuallyExclusive("prompt", "team-members")
	cmd.MarkFlagsOneRequired("prompt", "team-members")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)
	go func() {
		<-sigs
		fmt.Fprintln(os.Stderr, "\nAborted...")
		cancel()
	}()

	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

}

func createEvent(ctx context.Context, teamMembers []string, startDate time.Time, weeks int, eventName string) {
	b, err := ioutil.ReadFile("credentials.json")
	if err != nil {
		log.Fatalf("Unable to read client secret file: %v", err)
	}

	config, err := google.ConfigFromJSON(b, calendar.CalendarScope)
	if err != nil {
		log.Fatalf("Unable to parse client secret file to config: %v", err)
	}
	client := getClient(ctx, config)

	srv, err := calendar.New(client)
	if err != nil {
		log.Fatalf("Unable to retrieve Calendar client: %v", err)
	}

	// Slice calendars by name and ID.
	calendarList, err := srv.CalendarList.List().Do()
	if err != nil {
		log.Fatal("ERROR %w", err)
	}
	nameId := make(map[string]string)
	for _, v := range calendarList.Items {
		// log.Printf("Name: %s, ID: %s\n", v.Summary, v.Id)
		nameId[v.Summary] = v.Id
	}

	// Define calendar ID (primary calendar)
	calendarId := nameId["team-roles-test"]

	// Order the team members slice deterministically
	sort.Strings(teamMembers)

	// Convert duration to weeks
	durationInDays := int(weeks * 7)
	// Define the recurrence rule for every 3 weeks
	recurrenceRule := fmt.Sprintf("RRULE:FREQ=WEEKLY;INTERVAL=%v", weeks*len(teamMembers))

	// Create events for each team member
	for i, member := range teamMembers {
		memberStartDate := startDate.AddDate(0, 0, i*durationInDays)
		memberEndDate := memberStartDate.AddDate(0, 0, durationInDays)
		log.Printf("Creating event for %s starting on %v\n", member, memberStartDate)
		color := strconv.Itoa(i + 1)
		createRotationalEvent(srv, calendarId, fmt.Sprintf("%s: %s", eventName, member), memberStartDate, memberEndDate, recurrenceRule, color)
	}
}
