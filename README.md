# Girvi Webhook

A [Girvi](https://girvi.app/) plugin that posts loan events to a URL you choose:

- `loan.created`: a new loan was recorded
- `payment.received`: a payment was recorded that leaves principal owing
- `loan.redeemed`: a loan was paid off

It is a building block. Google Sheets, Zapier, Make, Slack alerts and accounting bridges can all start from a
webhook. Each firm has its own URL, and nothing is posted until you turn the plugin on for that firm.

## Install

Build it and copy it into Girvi's plugins folder (**Plugins, Build your own** shows where it is).

```bash
go build -o girvi-plugin-webhook .
# for Windows, from any system:
GOOS=windows GOARCH=amd64 go build -o girvi-plugin-webhook.exe .
```

Today Girvi only picks up files whose name starts with `girvi-plugin-`. In **Plugins**, press **Scan folder**,
review the new plugin and approve it. Approval is for that exact build. Then open **Settings**, enter the URL,
and turn the plugin on for a firm.

## Settings

| Setting | What it does |
|---|---|
| Webhook URL | Where to post. Use `https` unless it is on your own network. Plain `http` to the internet is refused unless you allow it under Advanced. |
| Signing secret | Optional. If set, every post is signed (see below). |
| Include customer names and phone numbers | Turn off to send only customer IDs. |
| Authorization header | Optional, sent as it is, for example `Bearer abc123`. |

## Try it

Run the example receiver, which checks the signature the way a real one should, and point Girvi at it:

```bash
go run ./examples/receiver -addr :9090 -secret s3cret
# Webhook URL:    http://127.0.0.1:9090/
# Signing secret: s3cret
```

Then press **Send test event** on the plugin in Girvi. It posts a loan with made-up details (customer
"Test Customer", loan `TEST-001`), so no real customer is involved, and the result shows in **Activity**.

## What is posted

A `POST` with `Content-Type: application/json`. Headers:

| Header | Value |
|---|---|
| `X-Girvi-Event` | `loan.created`, `payment.received` or `loan.redeemed` |
| `X-Girvi-Delivery` | The delivery id. Unique per event and the same on every retry. |
| `X-Girvi-Timestamp` | Unix seconds when the post was made. |
| `X-Girvi-Signature` | `v1=` and a hex HMAC-SHA256, only if a secret is set. |
| `Authorization` | Only if you set one. |

Body:

```json
{
  "id": "0193c1f2-...",
  "type": "loan.created",
  "version": 1,
  "occurred_at": "2026-11-07T05:11:48Z",
  "firm": { "id": "demo", "name": "Demo Firm" },
  "data": {
    "customer": { "id": "...", "name": "Ravi Kumar", "phone": "98765 43210", "phone_e164": "+919876543210" },
    "loan": {
      "id": "...", "number": "F-101", "status": "active", "total_pieces": 2,
      "pledged_on": { "iso": "2026-11-07", "display": "07 Nov 2026" },
      "principal": { "paise": 1500000, "display": "₹15,000.00" },
      "value": { "paise": 1750000, "display": "₹17,500.00" },
      "interest_rate": { "basis_points": 1400, "per": { "count": 1, "unit": "year" }, "display": "14% p.a." },
      "total_net_weight": { "milligrams": 25000, "display": "25.000 g" },
      "total_gross_weight": { "milligrams": 27500, "display": "27.500 g" }
    }
  }
}
```

`payment.received` has `customer`, `loan` (`id`, `number`), `amount`, `date` and `outstanding_principal`.
`loan.redeemed` has `customer`, `loan`, `redeemed_on` and `returned_weight`.

- Money is whole **paise** and weight is whole **milligrams**, as JSON numbers. The `display` strings are
  formatted by Girvi for you.
- ID document numbers are never included.
- Adding a field is compatible. Removing or renaming one raises `version`.

## Verify the signature

Compute HMAC-SHA256 with your secret over `timestamp + "." + body`, where body is the raw request body exactly as
received, and compare it to the header in constant time. Refuse a timestamp more than a few minutes old.

```python
import hmac, hashlib, time

def verify(secret: str, timestamp: str, signature: str, raw_body: bytes) -> bool:
    if abs(time.time() - int(timestamp)) > 300:
        return False
    mac = hmac.new(secret.encode(), timestamp.encode() + b"." + raw_body, hashlib.sha256)
    return hmac.compare_digest("v1=" + mac.hexdigest(), signature)
```

```js
const crypto = require("crypto");

function verify(secret, timestamp, signature, rawBody) {
  if (Math.abs(Date.now() / 1000 - Number(timestamp)) > 300) return false;
  const mac = crypto.createHmac("sha256", secret).update(`${timestamp}.${rawBody}`).digest("hex");
  const a = Buffer.from(`v1=${mac}`), b = Buffer.from(signature);
  return a.length === b.length && crypto.timingSafeEqual(a, b);
}
```

A Go version is in `examples/receiver/main.go`.

## Delivery

Delivery is at least once. Use `id` (or `X-Girvi-Delivery`) to ignore a repeat. Your answer decides what happens:

| Your answer | What Girvi does |
|---|---|
| `2xx` | Delivered. |
| `401`, `403`, `404`, `410`, or a redirect | Blocked: the event waits and is sent once you fix the URL or credentials. |
| Other `4xx` (for example `400`, `422`) | Failed, and shown in Activity. You can send it again from there. |
| `408`, `425`, `429`, `5xx`, or no answer | Retried later with increasing waits. |

Redirects are not followed, because a redirected `POST` would lose its body. Enter the final address.
An event that cannot be delivered within 7 days expires.

## Privacy

With "Include customer names and phone numbers" on, each post carries the customer's name and phone number, so
send it only to a receiver you trust and keep to `https`. Turn it off to send customer IDs only.
