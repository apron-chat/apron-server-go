package passkey

import "html/template"

// pageScript runs the passkey page's ceremony: on "Continue with passkey"
// (or "Create passkey"), it asks the page for WebAuthn options, calls
// navigator.credentials, has the page verify the result, and goes where the
// page says: the allow screen, or back to the client with a code.
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
    if (!response.ok) throw new Error(answer.error || "Something went wrong. Try again.");
    return answer;
  };
  button.addEventListener("click", async () => {
    button.disabled = true;
    status.textContent = "";
    try {
      if (!window.PublicKeyCredential) throw new Error("This browser can't use passkeys.");
      const { public_key: options } = await post("options");
      const publicKey = parse(options);
      const credential = create ? await navigator.credentials.create({ publicKey }) : await navigator.credentials.get({ publicKey });
      if (!credential) throw new Error("No passkey was chosen. Try again.");
      const answer = await post("verify", { credential: JSON.stringify(serialize(credential)) });
      window.location.assign(answer.redirect || answer.next);
    } catch (error) {
      status.textContent = error && error.name === "NotAllowedError"
        ? "The passkey prompt was closed. Try again, or cancel."
        : (error && error.message) || "Something went wrong. Try again.";
      button.disabled = false;
    }
  });
})();`)
