// Command girvi-webhook is a Girvi extension that posts loan events to a URL you
// choose: when a loan is created, a payment is received, or a loan is redeemed.
// It is the kind of building block that Google Sheets, Zapier, Make, Slack and
// accounting bridges are started from.
package main

import "github.com/anant-barlota/girvi-plugins/sdk"

func main() {
	sdk.Run(newWebhook())
}
