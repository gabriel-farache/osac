/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
*/

package billingaccount

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/osac-project/osac/fulfillment-service/internal/config"
	"github.com/osac-project/osac/fulfillment-service/internal/terminal"
	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
)

// Possible output formats:
const (
	outputFormatTable = "table"
	outputFormatJson  = "json"
	outputFormatYaml  = "yaml"
)

// Cmd creates the command to list the billing accounts discovered from the billing provider.
func Cmd() *cobra.Command {
	runner := &runnerContext{}
	result := &cobra.Command{
		Use:     "billingaccount",
		Aliases: []string{"billingaccounts"},
		Short:   shortHelp,
		Long:    longHelp,
		Example: example,
		Args:    cobra.NoArgs,
		RunE:    runner.run,
	}
	result.Flags().StringVarP(&runner.format, "output", "o", outputFormatTable, outputFlagHelp)
	return result
}

type runnerContext struct {
	format string
}

func (c *runnerContext) run(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	console := terminal.ConsoleFromContext(ctx)

	cfg := config.SettingsFromContext(ctx)
	if !cfg.Armed() {
		return fmt.Errorf("there is no configuration, run the 'login' command")
	}
	conn, err := cfg.Connect(ctx, cmd.Flags())
	if err != nil {
		return fmt.Errorf("failed to create gRPC connection: %w", err)
	}
	defer conn.Close()

	response, err := publicv1.NewBillingAccountsClient(conn).List(ctx, publicv1.BillingAccountsListRequest_builder{}.Build())
	if err != nil {
		return fmt.Errorf("failed to list billing accounts: %w", err)
	}
	return renderAccounts(ctx, console, response.GetItems(), c.format)
}

const shortHelp = `List the billing accounts of the billing provider`

const longHelp = `
List the accounts that exist in the configured billing provider, with the currency and state of each. The list is
refreshed from the provider on every call.

An account whose currency the provider does not supply shows {{ bt }}(unknown){{ bt }}, and an account whose state could
not be determined shows {{ bt }}UNKNOWN{{ bt }}. An empty list means that no accounts were discovered, not that billing
is disabled.
`

const example = `  # List the billing accounts
  {{ binary }} get billingaccount

  # List them as JSON
  {{ binary }} get billingaccount -o json`

const outputFlagHelp = `
_FORMAT_ - Output format. Must be one of {{ bt }}table{{ bt }}, {{ bt }}json{{ bt }} or {{ bt }}yaml{{ bt }}.
`

// renderAccounts writes the accounts in the given format.
func renderAccounts(ctx context.Context, console *terminal.Console, accounts []*publicv1.BillingAccount,
	format string) error {
	switch format {
	case outputFormatTable:
		renderAccountTable(console, accounts)
		return nil
	case outputFormatJson, outputFormatYaml:
		// Round-trip through protojson so the field and enum names are the ones of the API:
		values := make([]any, len(accounts))
		for i, account := range accounts {
			data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(account)
			if err != nil {
				return fmt.Errorf("failed to encode billing account: %w", err)
			}
			if err := json.Unmarshal(data, &values[i]); err != nil {
				return fmt.Errorf("failed to encode billing account: %w", err)
			}
		}
		if format == outputFormatJson {
			console.RenderJson(ctx, values)
		} else {
			console.RenderYaml(ctx, values)
		}
		return nil
	default:
		return fmt.Errorf(
			"unknown output format '%s', should be '%s', '%s' or '%s'",
			format, outputFormatTable, outputFormatJson, outputFormatYaml,
		)
	}
}

func renderAccountTable(w *terminal.Console, accounts []*publicv1.BillingAccount) {
	if len(accounts) == 0 {
		fmt.Fprintln(w, "No billing accounts found.")
		return
	}
	writer := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tPROVIDER\tNAME\tCURRENCY\tSTATE\tOBSERVED\tCORRELATION ID")
	for _, a := range accounts {
		currency := orDefault(a.GetBaseCurrency(), "(unknown)")
		state := "UNKNOWN"
		if a.GetState() != publicv1.BillingAccountState_BILLING_ACCOUNT_STATE_UNSPECIFIED {
			state = strings.TrimPrefix(a.GetState().String(), "BILLING_ACCOUNT_STATE_")
		}
		observed := "-"
		if a.HasStatusObservedAt() {
			observed = a.GetStatusObservedAt().AsTime().UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			a.GetId(),
			orDefault(a.GetProviderKey(), "-"),
			orDefault(a.GetDisplayName(), "-"),
			currency,
			state,
			observed,
			orDefault(a.GetCorrelationId(), "-"),
		)
	}
	writer.Flush()
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
