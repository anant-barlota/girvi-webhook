package main

import (
	"fmt"
	"strings"
	"time"

	deliveryv1 "github.com/anant-barlota/girvi-plugins/sdk/gen/girvi/delivery/v1"
	modelv1 "github.com/anant-barlota/girvi-plugins/sdk/gen/girvi/model/v1"
)

// The JSON below is this plugin's public contract with whatever receives it.
// It is written out as plain structs, not generated from the protobuf, so that
// numbers stay numbers (protobuf's JSON turns 64-bit integers into strings) and
// so that the shape only changes when this plugin says it does. Adding a field
// is compatible. Removing or renaming one bumps "version".
const payloadVersion = 1

type envelope struct {
	ID         string `json:"id"`      // the delivery id: unique per event, stable across retries
	Type       string `json:"type"`    // "loan.created", "payment.received" or "loan.redeemed"
	Version    int    `json:"version"` // of this payload shape
	OccurredAt string `json:"occurred_at"`
	Firm       firm   `json:"firm"`
	Data       any    `json:"data"`
}

type firm struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type money struct {
	Paise   int64  `json:"paise"` // integer paise: 150000 is ₹1,500.00
	Display string `json:"display"`
}

type weight struct {
	Milligrams int64  `json:"milligrams"`
	Display    string `json:"display"`
}

type date struct {
	ISO     string `json:"iso"` // 2026-10-12
	Display string `json:"display"`
}

type duration struct {
	Count uint32 `json:"count"`
	Unit  string `json:"unit"` // "year" or "month"
}

type rate struct {
	BasisPoints uint32   `json:"basis_points"` // hundredths of a percent: 1400 is 14%
	Per         duration `json:"per"`
	Display     string   `json:"display"`
}

type customer struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	Phone     string `json:"phone,omitempty"`
	PhoneE164 string `json:"phone_e164,omitempty"`
}

type loan struct {
	ID               string  `json:"id"`
	Number           string  `json:"number"`
	PledgedOn        *date   `json:"pledged_on,omitempty"`
	Principal        *money  `json:"principal,omitempty"`
	Value            *money  `json:"value,omitempty"`
	InterestRate     *rate   `json:"interest_rate,omitempty"`
	TotalNetWeight   *weight `json:"total_net_weight,omitempty"`
	TotalGrossWeight *weight `json:"total_gross_weight,omitempty"`
	TotalPieces      int64   `json:"total_pieces"`
	Status           string  `json:"status"`
}

type loanRef struct {
	ID     string `json:"id"`
	Number string `json:"number"`
}

type loanCreated struct {
	Customer customer `json:"customer"`
	Loan     loan     `json:"loan"`
}

type paymentReceived struct {
	Customer             customer `json:"customer"`
	Loan                 loanRef  `json:"loan"`
	Amount               *money   `json:"amount,omitempty"`
	Date                 *date    `json:"date,omitempty"`
	OutstandingPrincipal *money   `json:"outstanding_principal,omitempty"`
}

type loanRedeemed struct {
	Customer       customer `json:"customer"`
	Loan           loanRef  `json:"loan"`
	RedeemedOn     *date    `json:"redeemed_on,omitempty"`
	ReturnedWeight *weight  `json:"returned_weight,omitempty"`
}

func toMoney(m *modelv1.Money) *money {
	if m == nil {
		return nil
	}
	return &money{Paise: m.GetPaise(), Display: m.GetDisplay()}
}

func toWeight(w *modelv1.Weight) *weight {
	if w == nil {
		return nil
	}
	return &weight{Milligrams: w.GetMilligrams(), Display: w.GetDisplay()}
}

func toDate(d *modelv1.Date) *date {
	if d == nil {
		return nil
	}
	return &date{ISO: d.GetIso(), Display: d.GetDisplay()}
}

func toRate(r *modelv1.Rate) *rate {
	if r == nil {
		return nil
	}
	unit := "year"
	if r.GetPer().GetUnit() == modelv1.DurationUnit_DURATION_UNIT_MONTH {
		unit = "month"
	}
	return &rate{
		BasisPoints: r.GetBasisPoints(),
		Per:         duration{Count: r.GetPer().GetCount(), Unit: unit},
		Display:     r.GetDisplay(),
	}
}

// toCustomer leaves out the person's details unless the shop chose to share them.
func toCustomer(c *modelv1.Customer, includeDetails bool) customer {
	out := customer{ID: c.GetId()}
	if includeDetails {
		out.Name, out.Phone, out.PhoneE164 = c.GetDisplayName(), c.GetPhone(), c.GetPhoneE164()
	}
	return out
}

func loanStatus(s modelv1.LoanStatus) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "LOAN_STATUS_"))
}

func toLoan(l *modelv1.Loan) loan {
	return loan{
		ID: l.GetId(), Number: l.GetNumber(),
		PledgedOn: toDate(l.GetPledgedOn()), Principal: toMoney(l.GetPrincipal()), Value: toMoney(l.GetValue()),
		InterestRate:   toRate(l.GetInterestRate()),
		TotalNetWeight: toWeight(l.GetTotalNetWeight()), TotalGrossWeight: toWeight(l.GetTotalGrossWeight()),
		TotalPieces: l.GetTotalPieces(), Status: loanStatus(l.GetStatus()),
	}
}

func toLoanRef(l *modelv1.LoanRef) loanRef { return loanRef{ID: l.GetId(), Number: l.GetNumber()} }

// build turns one delivery into the JSON envelope.
func build(item *deliveryv1.Item, firmName string, includeDetails bool) (envelope, error) {
	env := envelope{
		ID: item.GetDeliveryId(), Version: payloadVersion,
		OccurredAt: item.GetOccurredAt().AsTime().UTC().Format(time.RFC3339),
		Firm:       firm{ID: item.GetFirmId(), Name: firmName},
	}
	switch {
	case item.GetLoanCreated() != nil:
		e := item.GetLoanCreated()
		env.Type = "loan.created"
		env.Data = loanCreated{Customer: toCustomer(e.GetCustomer(), includeDetails), Loan: toLoan(e.GetLoan())}
	case item.GetPaymentReceived() != nil:
		e := item.GetPaymentReceived()
		env.Type = "payment.received"
		env.Data = paymentReceived{
			Customer: toCustomer(e.GetCustomer(), includeDetails), Loan: toLoanRef(e.GetLoan()),
			Amount: toMoney(e.GetAmount()), Date: toDate(e.GetDate()), OutstandingPrincipal: toMoney(e.GetOutstandingPrincipal()),
		}
	case item.GetLoanRedeemed() != nil:
		e := item.GetLoanRedeemed()
		env.Type = "loan.redeemed"
		env.Data = loanRedeemed{
			Customer: toCustomer(e.GetCustomer(), includeDetails), Loan: toLoanRef(e.GetLoan()),
			RedeemedOn: toDate(e.GetRedeemedOn()), ReturnedWeight: toWeight(e.GetReturnedWeight()),
		}
	default:
		return envelope{}, fmt.Errorf("this plugin sends loan events only, not %q", item.GetCapability())
	}
	return env, nil
}
