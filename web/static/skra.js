// SPDX-License-Identifier: AGPL-3.0-or-later
"use strict";

// The server puts a per-process token into the page; every POST sends it back.
const token = document.querySelector('meta[name="skra-token"]').content;

// post sends body as JSON, or as it is when it is a FormData, with the token.
async function post(url, body) {
  const headers = { "X-Skra-Token": token };
  let payload = body;
  if (!(body instanceof FormData)) {
    headers["Content-Type"] = "application/json";
    payload = JSON.stringify(body ?? {});
  }
  return fetch(url, { method: "POST", headers, body: payload });
}
