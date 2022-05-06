package ring

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

type StatusPageData struct {
	Ingesters  []IngesterDesc `json:"shards"`
	Now        time.Time      `json:"now"`
	ShowTokens bool           `json:"-"`
}

type IngesterDesc struct {
	ID                  string    `json:"id"`
	State               string    `json:"state"`
	Address             string    `json:"address"`
	HeartbeatTimestamp  time.Time `json:"timestamp"`
	RegisteredTimestamp time.Time `json:"registered_timestamp"`
	Zone                string    `json:"zone"`
	Tokens              []uint32  `json:"tokens"`
	NumTokens           int       `json:"-"`
	Ownership           float64   `json:"-"`
}

// Operator allows external entities to perform generic operations on the ring,
// like describing it or force-forgetting one of the members.
type Operator interface {
	Describe(ctx context.Context) (*Desc, error)
	Forget(ctx context.Context, id string) error
}

type ringPageHandler struct {
	r               Operator
	heartbeatPeriod time.Duration
}

func newRingPageHandler(r Operator, heartbeatPeriod time.Duration) *ringPageHandler {
	return &ringPageHandler{
		r:               r,
		heartbeatPeriod: heartbeatPeriod,
	}
}

func (h *ringPageHandler) handle(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodPost {
		ingesterID := req.FormValue("forget")
		if err := h.r.Forget(req.Context(), ingesterID); err != nil {
			http.Error(
				w,
				fmt.Errorf("error forgetting instance '%s': %w", ingesterID, err).Error(),
				http.StatusInternalServerError,
			)
			return
		}

		// Implement PRG pattern to prevent double-POST and work with CSRF middleware.
		// https://en.wikipedia.org/wiki/Post/Redirect/Get

		// http.Redirect() would convert our relative URL to absolute, which is not what we want.
		// Browser knows how to do that, and it also knows real URL. Furthermore it will also preserve tokens parameter.
		// Note that relative Location URLs are explicitly allowed by specification, so we're not doing anything wrong here.
		w.Header().Set("Location", "#")
		w.WriteHeader(http.StatusFound)

		return
	}

	ringDesc, err := h.r.Describe(req.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ownedTokens := ringDesc.countTokens()

	var ingesterIDs []string
	for id := range ringDesc.Ingesters {
		ingesterIDs = append(ingesterIDs, id)
	}
	sort.Strings(ingesterIDs)

	now := time.Now()
	var ingesters []IngesterDesc
	for _, id := range ingesterIDs {
		ing := ringDesc.Ingesters[id]
		state := ing.State.String()
		if !ing.IsHealthy(Reporting, h.heartbeatPeriod, now) {
			state = "UNHEALTHY"
		}

		ingesters = append(ingesters, IngesterDesc{
			ID:                  id,
			State:               state,
			Address:             ing.Addr,
			HeartbeatTimestamp:  time.Unix(ing.Timestamp, 0).UTC(),
			RegisteredTimestamp: ing.GetRegisteredAt().UTC(),
			Tokens:              ing.Tokens,
			Zone:                ing.Zone,
			NumTokens:           len(ing.Tokens),
			Ownership:           (float64(ownedTokens[id]) / float64(math.MaxUint32)) * 100,
		})
	}

	tokensParam := req.URL.Query().Get("tokens")

	renderHTTPResponse(w, StatusPageData{
		Ingesters:  ingesters,
		Now:        now,
		ShowTokens: tokensParam == "true",
	}, defaultPageTemplate, req)
}

// RenderHTTPResponse either responds with json or a rendered html page using the passed in template
// by checking the Accepts header
func renderHTTPResponse(w http.ResponseWriter, v StatusPageData, t *template.Template, r *http.Request) {
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") {
		writeJSONResponse(w, v)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	if err := t.Execute(w, v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// WriteJSONResponse writes some JSON as a HTTP response.
func writeJSONResponse(w http.ResponseWriter, v StatusPageData) {
	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

//go:embed status.gohtml
var defaultPageContent string
var defaultPageTemplate = template.Must(template.New("webpage").Funcs(template.FuncMap{
	"mod": func(i, j int) bool { return i%j == 0 },
	"humanFloat": func(f float64) string {
		return fmt.Sprintf("%.2g", f)
	},
	"timeOrEmptyString": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Format(time.RFC3339Nano)
	},
	"durationSince": func(t time.Time) string { return time.Since(t).Truncate(time.Millisecond).String() },
}).Parse(defaultPageContent))
