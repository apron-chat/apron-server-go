package passkey

import "html/template"

// pageScript runs the passkey page's ceremony. It asks the page for
// WebAuthn options as the page shows, and again after each attempt that
// fails or is dismissed, so "Continue with passkey" (or "Create passkey")
// calls navigator.credentials at once, within the tap's user activation:
// Safari refuses a ceremony, or asks for another tap, once the activation
// is spent waiting on a request. Options are held only for the first half
// of their timeout, the ceremony's lifetime, so a ceremony begun with them
// has at least the other half; a timer asks for new ones then. A tap on
// options past that, as when a background tab held the timer back, asks
// for new ones and says to tap again. The button waits while options are
// asked for, since each request replaces the ceremony the page holds.
//
// It has the page verify the credential, and goes back to the client with
// the code the page answers with. When the page answers that no account
// has the passkey (unknown_credential, only with Scheme.SignalUnknown), it
// asks the passkey provider to hide it, where the browser can, and stays
// on the page.
const pageScript = template.JS(`(() => {
  const main = document.querySelector("main");
  const button = document.getElementById("passkey");
  const status = document.getElementById("status");
  if (!main || !button) return;
  const create = main.dataset.ceremony === "create";
  const bytes = (text) => {
    const base64 = text.replace(/-/g, "+").replace(/_/g, "/");
    return Uint8Array.from(atob(base64 + "===".slice((base64.length + 3) % 4)), (c) => c.charCodeAt(0));
  };
  const text = (buffer) => {
    let binary = "";
    for (const byte of new Uint8Array(buffer)) binary += String.fromCharCode(byte);
    return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  };
  const parse = (options) => {
    if (create && PublicKeyCredential.parseCreationOptionsFromJSON) return PublicKeyCredential.parseCreationOptionsFromJSON(options);
    if (!create && PublicKeyCredential.parseRequestOptionsFromJSON) return PublicKeyCredential.parseRequestOptionsFromJSON(options);
    const parsed = { ...options, challenge: bytes(options.challenge) };
    if (create) {
      parsed.user = { ...options.user, id: bytes(options.user.id) };
      parsed.excludeCredentials = (options.excludeCredentials || []).map((c) => ({ ...c, id: bytes(c.id) }));
    } else {
      parsed.allowCredentials = (options.allowCredentials || []).map((c) => ({ ...c, id: bytes(c.id) }));
    }
    return parsed;
  };
  const serialize = (credential) => {
    try {
      if (credential.toJSON) return credential.toJSON();
    } catch (_) {}
    const r = credential.response;
    const response = { clientDataJSON: text(r.clientDataJSON) };
    if (create) {
      response.attestationObject = text(r.attestationObject);
      if (r.getTransports) response.transports = r.getTransports();
    } else {
      response.authenticatorData = text(r.authenticatorData);
      response.signature = text(r.signature);
      if (r.userHandle) response.userHandle = text(r.userHandle);
    }
    const json = { id: credential.id, rawId: text(credential.rawId), type: credential.type, response, clientExtensionResults: credential.getClientExtensionResults() };
    if (credential.authenticatorAttachment) json.authenticatorAttachment = credential.authenticatorAttachment;
    return json;
  };
  const post = async (op, extra) => {
    const response = await fetch(main.dataset.post, {
      method: "POST",
      body: new URLSearchParams({ flow: main.dataset.flow, op, ...extra }),
      credentials: "same-origin",
    });
    const answer = await response.json().catch(() => ({}));
    if (!response.ok) throw Object.assign(new Error(answer.error || "Something went wrong. Try again."), { unknownCredential: answer.unknown_credential === true });
    return answer;
  };
  // prepared holds the options a tap uses, and until when; preparing is
  // the request for new ones, which resolves to its error, if any.
  let prepared = null;
  let preparing = null;
  let refresh;
  const prepare = () => {
    clearTimeout(refresh);
    prepared = null;
    button.disabled = true;
    const mine = preparing = post("options").then(({ public_key: options }) => {
      if (preparing !== mine) return;
      const held = options.timeout / 2 || 30000;
      prepared = { options, until: Date.now() + held };
      refresh = setTimeout(prepare, held);
    }, (error) => error).then((error) => {
      if (preparing !== mine) return error;
      preparing = null;
      button.disabled = false;
      if (error && !status.textContent) status.textContent = error.message;
      return error;
    });
    return mine;
  };
  button.addEventListener("click", async () => {
    status.textContent = "";
    if (!window.PublicKeyCredential) {
      status.textContent = "This browser can't use passkeys.";
      return;
    }
    if (!prepared || Date.now() >= prepared.until) {
      // Asking now would spend the tap's activation: ask, then for
      // another tap.
      const error = await prepare();
      status.textContent = error ? error.message : "Ready. Press “" + button.textContent + "” again.";
      return;
    }
    const { options } = prepared;
    prepared = null;
    clearTimeout(refresh);
    button.disabled = true;
    try {
      const publicKey = parse(options);
      const credential = create ? await navigator.credentials.create({ publicKey }) : await navigator.credentials.get({ publicKey });
      if (!credential) throw new Error("No passkey was chosen. Try again.");
      let answer;
      try {
        answer = await post("verify", { credential: JSON.stringify(serialize(credential)) });
      } catch (error) {
        // No account here has this passkey: the passkey provider may hide
        // it, so it is not offered again. Its answer tells nothing.
        if (error.unknownCredential && !create && options.rpId && PublicKeyCredential.signalUnknownCredential) {
          try {
            Promise.resolve(PublicKeyCredential.signalUnknownCredential({ rpId: options.rpId, credentialId: credential.id })).catch(() => {});
          } catch (_) {}
        }
        throw error;
      }
      // A verified passkey ends the flow: nothing more to prepare.
      window.location.assign(answer.redirect);
    } catch (error) {
      status.textContent = error && error.name === "NotAllowedError"
        ? "The passkey prompt was closed. Try again, or cancel."
        : (error && error.message) || "Something went wrong. Try again.";
      prepare();
    }
  });
  if (window.PublicKeyCredential) prepare();
})();`)
