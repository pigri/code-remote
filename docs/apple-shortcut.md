# Apple Shortcuts for code-remote

Drive the remote API from iPhone / iPad / Mac / Apple Watch using the built-in
**Shortcuts** app — no extra app required. Each shortcut is just a
`Get Contents of URL` call against your public endpoint (ngrok → Synapse WAF →
API), authenticated with the bearer token.

These hit the **public** URL, so they work from anywhere:

```
https://your-domain.ngrok.dev/sessions
```

Replace `your-domain.ngrok.dev` with your real reserved domain and use your
`CLAUDE_REMOTE_API_TOKEN` value.

> The token lives on-device inside the shortcut — treat the shortcut like a
> password. Anyone who can run it can create/stop sessions. The WAF + bearer
> auth still gate every request.

A tidy pattern: put the base URL and token in **two text actions at the top** of
each shortcut (or, better, in a single shared shortcut you call), so there's one
place to update.

---

## 1) New session

| # | Action | Configuration |
| - | --- | --- |
| 1 | **Text** | your token → name the output `Token` |
| 2 | **Get Contents of URL** | URL `https://your-domain.ngrok.dev/sessions`; Method **POST**; Header `Authorization` = `Bearer ` + `Token`; Request Body **JSON** `{}` |
| 3 | **Get Dictionary Value** | Value for `id` in **Contents of URL** |
| 4 | **Show Notification** | `Started session [id]` |

Returns `201` with the session object; `id` is the Claude session UUID **and**
the Remote Control name, so you can reconnect to it from the Claude app/CLI.

## 2) List sessions

| # | Action | Configuration |
| - | --- | --- |
| 1 | **Text** | your token → `Token` |
| 2 | **Get Contents of URL** | URL `…/sessions`; Method **GET**; Header `Authorization` = `Bearer ` + `Token` |
| 3 | **Repeat with Each** | item in **Contents of URL** |
| 4 | &nbsp;&nbsp;**Get Dictionary Value** | `title` (and `id`) from **Repeat Item** |
| 5 | **Show Result** / **Choose from List** | show titles; feed the chosen `id` into the Stop shortcut below |

## 3) Stop a session

| # | Action | Configuration |
| - | --- | --- |
| 1 | **Text** | your token → `Token` |
| 2 | **Ask for Input** (or pass an `id` in) | the session UUID to stop |
| 3 | **Text** | `https://your-domain.ngrok.dev/sessions/` + the id → `URL` |
| 4 | **Get Contents of URL** | URL = `URL`; Method **DELETE**; Header `Authorization` = `Bearer ` + `Token` |
| 5 | **Show Notification** | `Stopped [id]` |

Tip: chain **List → Choose from List → Stop** into one shortcut for a
pick-and-kill flow.

---

## What the shortcuts do (curl equivalents)

```sh
TOKEN=...   # CLAUDE_REMOTE_API_TOKEN
BASE=https://your-domain.ngrok.dev

# new
curl -s -X POST   -H "Authorization: Bearer $TOKEN" "$BASE/sessions"
# list
curl -s           -H "Authorization: Bearer $TOKEN" "$BASE/sessions"
# stop
curl -s -X DELETE -H "Authorization: Bearer $TOKEN" "$BASE/sessions/<id>"
```

If a call returns `401`, the token/header is wrong; `403` means the Synapse WAF
blocked the request (e.g. a suspicious payload). On success you get `201` (new),
`200` (list), or `204` (stop).
