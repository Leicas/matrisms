package connector

import (
	"context"
	"fmt"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"

	"github.com/Leicas/matrisms/pkg/common"
	"github.com/Leicas/matrisms/pkg/voipms"
)

const LoginFlowIDAPIPassword = "voipms-api-password"

// SMSLoginProcess drives the interactive login:
// credentials → did_selection → complete.
type SMSLoginProcess struct {
	user      *bridgev2.User
	connector *SMSConnector

	apiUsername string
	apiPassword string

	currentStep   string
	availableDIDs []voipms.DID
	selectedDIDs  []string
}

var (
	_ bridgev2.LoginProcess          = (*SMSLoginProcess)(nil)
	_ bridgev2.LoginProcessUserInput = (*SMSLoginProcess)(nil)
)

func (lp *SMSLoginProcess) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	lp.currentStep = "credentials"
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: "credentials",
		Instructions: `**VoIP.ms API login**

1. Go to **voip.ms → Main Menu → SOAP & REST/JSON API**
2. Set an **API password** (this is NOT your portal password)
3. **Enable** the API
4. Add this server's IP to **Enable IP Addresses** (or 0.0.0.0 to allow all)

Then enter your credentials below.`,
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{
				{
					Type:        bridgev2.LoginInputFieldTypeEmail,
					ID:          "username",
					Name:        "Account email",
					Description: "The email you log into voip.ms with",
				},
				{
					Type:        bridgev2.LoginInputFieldTypePassword,
					ID:          "password",
					Name:        "API password",
					Description: "The API password from Main Menu → SOAP & REST/JSON API",
				},
			},
		},
	}, nil
}

func (lp *SMSLoginProcess) Cancel() {
	lp.currentStep = ""
}

func (lp *SMSLoginProcess) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	switch lp.currentStep {
	case "credentials":
		return lp.handleCredentials(ctx, input)
	case "did_selection":
		return lp.handleDIDSelection(ctx, input)
	default:
		return nil, fmt.Errorf("login process is not at a user-input step (at %q)", lp.currentStep)
	}
}

func (lp *SMSLoginProcess) handleCredentials(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	lp.apiUsername = strings.ToLower(strings.TrimSpace(input["username"]))
	lp.apiPassword = strings.TrimSpace(input["password"])
	if lp.apiUsername == "" || lp.apiPassword == "" {
		return nil, fmt.Errorf("both the account email and the API password are required")
	}

	log := lp.connector.Bridge.Log.With().Str("component", "login").Logger()
	client := voipms.NewClient(lp.connector.Config.VoIPms.EffectiveBaseURL(), lp.apiUsername, lp.apiPassword, &log)

	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dids, err := client.VerifyCredentials(checkCtx)
	if err != nil {
		if voipms.IsAuthError(err) {
			return nil, fmt.Errorf("VoIP.ms rejected the credentials (%v). Check the API password, that the API is enabled, and that this server's IP is whitelisted", err)
		}
		return nil, fmt.Errorf("could not reach the VoIP.ms API: %w", err)
	}
	if len(dids) == 0 {
		return nil, fmt.Errorf("credentials are valid, but no SMS-capable DIDs were found on the account. Enable SMS on a DID in the VoIP.ms portal first")
	}
	lp.availableDIDs = dids
	lp.currentStep = "did_selection"

	var list strings.Builder
	for i, d := range dids {
		state := "SMS"
		if d.MMSEnabled {
			state = "SMS+MMS"
		}
		desc := d.Description
		if desc != "" {
			desc = " — " + desc
		}
		fmt.Fprintf(&list, "\n%d. **%s** (%s)%s", i+1, common.FormatPhone(d.Number), state, desc)
	}

	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: "did_selection",
		Instructions: fmt.Sprintf(`✅ Credentials verified. SMS-capable numbers on your account:%s

Which numbers should be bridged? Enter **all**, or a comma-separated list of indexes or numbers (e.g. `+"`1,3` or `5551234567`"+`).`, list.String()),
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{
				{
					Type:        bridgev2.LoginInputFieldTypeUsername,
					ID:          "dids",
					Name:        "Numbers to bridge",
					Description: "'all', or comma-separated indexes/numbers",
				},
			},
		},
	}, nil
}

func (lp *SMSLoginProcess) handleDIDSelection(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	choice := strings.ToLower(strings.TrimSpace(input["dids"]))
	if choice == "" || choice == "all" {
		for _, d := range lp.availableDIDs {
			lp.selectedDIDs = append(lp.selectedDIDs, d.Number)
		}
	} else {
		for _, tok := range strings.Split(choice, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			matched := ""
			// Try as 1-based index first.
			var idx int
			if _, err := fmt.Sscanf(tok, "%d", &idx); err == nil && idx >= 1 && idx <= len(lp.availableDIDs) && len(tok) <= 3 {
				matched = lp.availableDIDs[idx-1].Number
			} else {
				norm := common.NormalizePhone(tok)
				for _, d := range lp.availableDIDs {
					if d.Number == norm {
						matched = d.Number
						break
					}
				}
			}
			if matched == "" {
				return nil, fmt.Errorf("%q doesn't match any listed number — enter 'all', an index, or a full number", tok)
			}
			lp.selectedDIDs = append(lp.selectedDIDs, matched)
		}
	}
	if len(lp.selectedDIDs) == 0 {
		return nil, fmt.Errorf("no numbers selected")
	}
	return lp.completeLogin(ctx)
}

func (lp *SMSLoginProcess) completeLogin(ctx context.Context) (*bridgev2.LoginStep, error) {
	// Save credentials FIRST so LoadUserLogin (triggered by NewLogin) finds them.
	account := &SMSAccount{
		UserMXID:    lp.user.MXID.String(),
		APIUsername: lp.apiUsername,
		APIPassword: lp.apiPassword,
		DIDs:        lp.selectedDIDs,
		CreatedAt:   time.Now(),
	}
	if err := lp.connector.DB.UpsertAccount(ctx, account); err != nil {
		return nil, fmt.Errorf("failed to save account: %w", err)
	}

	userLogin, err := lp.user.NewLogin(ctx, &database.UserLogin{
		ID:         common.UserLoginIDFor(lp.apiUsername),
		RemoteName: lp.apiUsername,
		Metadata: &SMSLoginMetadata{
			APIUsername: lp.apiUsername,
		},
	}, &bridgev2.NewLoginParams{
		DeleteOnConflict: false,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create user login: %w", err)
	}

	// The framework only calls Client.Connect in StartLogins at bridge
	// startup; logins created interactively must start their own
	// connection (this is what kicks off the poller).
	if userLogin.Client != nil {
		go userLogin.Client.Connect(userLogin.Log.WithContext(context.Background()))
	}

	var numberList strings.Builder
	for _, did := range lp.selectedDIDs {
		fmt.Fprintf(&numberList, "\n  • %s", common.FormatPhone(did))
	}
	successMsg := fmt.Sprintf(`✅ **VoIP.ms account connected!**

📱 **Bridged numbers:**%s

Incoming texts will create rooms here as they arrive. To text someone new, use `+"`!matrisms text <number> <message>`"+`.

💡 For instant delivery (instead of polling), enable the webhook in the bridge config and point the VoIP.ms "SMS URL Callback" for each DID at it.`, numberList.String())

	lp.currentStep = "complete"
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       "complete",
		Instructions: successMsg,
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: userLogin.ID,
			UserLogin:   userLogin,
		},
	}, nil
}
