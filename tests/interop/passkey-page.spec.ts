import { createHash, randomBytes } from 'node:crypto';
import { expect, test, type BrowserContext, type Page } from '@playwright/test';
import { SERVER_TITLE } from './server-look';

// The passkey page (SERVER.md, "The passkey page") in a real browser, with Chromium's virtual
// authenticator: an app that cannot use the RP ID, a protocol client here with no Origin, adds a
// passkey to its guest on the page, then signs in with it there, and finishes each with the code.
// Only aprond takes part; the server is reached at localhost, its default RP ID and an RP origin.

const server = 'ws://localhost:8080/ws';
// The app's redirect target, a web client on another domain, which the test answers for it.
const redirectURI = 'https://app.example/signed-in';
const clientID = 'https://app.example/';

type Frame = { id?: string; method?: string; result?: Record<string, any>; error?: { code: number; message: string } };

/** A small protocol client: requests in order, notifications ignored. */
class Connection {
	private next = 0;
	private waiting = new Map<string, (frame: Frame) => void>();
	private constructor(private socket: WebSocket) {
		socket.addEventListener('message', (event) => {
			const frame = JSON.parse(String(event.data)) as Frame;
			if (frame.id !== undefined) this.waiting.get(frame.id)?.(frame);
		});
	}

	static open(): Promise<Connection> {
		const socket = new WebSocket(server);
		return new Promise((resolve, reject) => {
			socket.addEventListener('open', () => resolve(new Connection(socket)));
			socket.addEventListener('error', () => reject(new Error(`cannot connect to ${server}`)));
		});
	}

	async request(method: string, params: Record<string, unknown>): Promise<Record<string, any>> {
		const id = `r${++this.next}`;
		const reply = new Promise<Frame>((resolve) => this.waiting.set(id, resolve));
		this.socket.send(JSON.stringify({ method, id, params }));
		const frame = await reply;
		if (!frame.result) throw new Error(`${method}: ${JSON.stringify(frame.error)}`);
		return frame.result;
	}

	close() {
		this.socket.close();
	}
}

/** The app's authorization request for the page a begin named (PROTOCOL.md §4.12.2). */
function authorize(page: string) {
	const verifier = randomBytes(32).toString('base64url');
	const state = randomBytes(16).toString('base64url');
	const url = new URL(page);
	url.searchParams.set('response_type', 'code');
	url.searchParams.set('client_id', clientID);
	url.searchParams.set('redirect_uri', redirectURI);
	url.searchParams.set('code_challenge', createHash('sha256').update(verifier).digest('base64url'));
	url.searchParams.set('code_challenge_method', 'S256');
	url.searchParams.set('state', state);
	url.searchParams.set('resource', url.origin);
	return { url: url.toString(), verifier, state };
}

/**
 * Presses a button that returns to the app, and returns the code after checking state. The return
 * may be a redirect, which only its request shows.
 */
async function returned(page: Page, state: string, press: () => Promise<void>): Promise<string> {
	const back = page.waitForRequest((request) => request.url().startsWith(redirectURI));
	await press();
	const returning = await back;
	const url = new URL(returning.url());
	// Let that navigation end (the route doesn't answer a redirect's request, so it fails), or the
	// test's next goto races it.
	await returning.response();
	await page.waitForLoadState();
	expect(url.searchParams.get('state')).toBe(state);
	expect(url.searchParams.get('iss')).toBe('http://localhost:8080');
	return url.searchParams.get('code')!;
}

test('an app adds a passkey on the passkey page, then signs in with it there', async ({ page, context }) => {
	const cdp = await context.newCDPSession(page);
	await cdp.send('WebAuthn.enable');
	await cdp.send('WebAuthn.addVirtualAuthenticator', {
		options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true }
	});
	await page.route('https://app.example/**', (route) => route.fulfill({ contentType: 'text/plain', body: 'Back in the app' }));

	// The guest begins an addition, which only the page can run for it.
	const guest = await Connection.open();
	const you = (await guest.request('auth', { scheme: 'guest', name: 'Grace' })).you;
	const adding = await guest.request('auth', { scheme: 'webauthn', action: 'register', step: 'begin' });
	expect(adding.public_key).toBeUndefined();
	expect(adding.external.url).toMatch(/^http:\/\/localhost:8080\/passkey\?action=register&request=/);

	let request = authorize(adding.external.url);
	await page.goto(request.url);
	await expect(page.getByRole('heading', { name: 'Add a passkey' })).toBeVisible();
	await expect(page.getByText(`app.example wants to add a passkey to Grace (@${you.user_id})’s account on localhost:8080.`)).toBeVisible();
	await expect(page.getByText('localhost:8080 has not verified this app.')).toBeVisible();
	let code = await returned(page, request.state, () => page.getByRole('button', { name: 'Create passkey' }).click());
	const added = await guest.request('auth', {
		scheme: 'webauthn', action: 'register', step: 'finish', code, code_verifier: request.verifier, redirect_uri: redirectURI, client_id: clientID
	});
	expect(added.you).toMatchObject({ user_id: you.user_id, name: 'Grace' });
	expect(typeof added.token).toBe('string');
	guest.close();

	// Another connection signs in with that passkey on the page.
	const app = await Connection.open();
	const login = await app.request('auth', { scheme: 'webauthn', action: 'login', step: 'begin' });
	request = authorize(login.external.url);
	await page.goto(request.url);
	// One screen before the passkey; a verified passkey returns to the app at once.
	await expect(page.getByRole('heading', { name: `Sign in to ${SERVER_TITLE} on app.example?` })).toBeVisible();
	await expect(page.getByText('It will be able to read and send messages as you.')).toBeVisible();
	await expect(page.getByText('localhost:8080 has not verified this app.')).toBeVisible();
	await expect(page.getByText('Only continue if you just started signing in on app.example.')).toBeVisible();
	code = await returned(page, request.state, () => page.getByRole('button', { name: 'Continue with passkey' }).click());
	const signedIn = await app.request('auth', {
		scheme: 'webauthn', action: 'login', step: 'finish', code, code_verifier: request.verifier, redirect_uri: redirectURI, client_id: clientID
	});
	expect(signedIn.you.user_id).toBe(you.user_id);
	app.close();
});

test('a passkey no account has is signalled unknown on the passkey page, which stays open', async ({ page, context }) => {
	const cdp = await context.newCDPSession(page);
	await cdp.send('WebAuthn.enable');
	const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', {
		options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true }
	});
	await page.route('https://app.example/**', (route) => route.fulfill({ contentType: 'text/plain', body: 'Back in the app' }));
	// Records the page's signals, whether or not this browser has the Signal API.
	await page.addInitScript(() => {
		const signalled: unknown[] = ((window as any).signalled = []);
		const original = (PublicKeyCredential as any).signalUnknownCredential?.bind(PublicKeyCredential);
		(PublicKeyCredential as any).signalUnknownCredential = (options: unknown) => {
			signalled.push(options);
			return original ? original(options) : Promise.resolve();
		};
	});

	// A passkey made on the page, whose code is burned: it is on the device, and no account has it.
	const guest = await Connection.open();
	await guest.request('auth', { scheme: 'guest', name: 'Orphan' });
	const adding = await guest.request('auth', { scheme: 'webauthn', action: 'register', step: 'begin' });
	let request = authorize(adding.external.url);
	await page.goto(request.url);
	const code = await returned(page, request.state, () => page.getByRole('button', { name: 'Create passkey' }).click());
	await expect(
		guest.request('auth', {
			scheme: 'webauthn', action: 'register', step: 'finish', code, code_verifier: randomBytes(32).toString('base64url'), redirect_uri: redirectURI, client_id: clientID
		})
	).rejects.toThrow();
	guest.close();
	const { credentials } = await cdp.send('WebAuthn.getCredentials', { authenticatorId });
	expect(credentials).toHaveLength(1);

	// Signing in with it: the page signals it unknown, says so, and stays, to try again or cancel.
	const app = await Connection.open();
	const login = await app.request('auth', { scheme: 'webauthn', action: 'login', step: 'begin' });
	app.close();
	request = authorize(login.external.url);
	await page.goto(request.url);
	await page.getByRole('button', { name: 'Continue with passkey' }).click();
	await expect(page.getByText("This passkey isn't for an account on localhost:8080. Try another passkey, or cancel.")).toBeVisible();
	expect(await page.evaluate(() => (window as any).signalled)).toEqual([
		{ rpId: 'localhost', credentialId: Buffer.from(credentials[0].credentialId, 'base64').toString('base64url') }
	]);
	expect(new URL(page.url()).pathname).toBe('/passkey');
	await expect(page.getByRole('button', { name: 'Continue with passkey' })).toBeEnabled();
	await expect(page.getByRole('button', { name: 'Cancel' })).toBeVisible();
});

// The web client on an origin that cannot use the RP ID: the app at 127.0.0.1 (the config's
// baseURL) connects to the server at localhost, whose passkeys are for localhost. Its passkey
// ceremonies go to the server's page, in a popup, or in the app's own tab when the browser blocks
// one, and come back to the app's /signed-in/ with the code it finishes webauthn with.

/** A virtual authenticator on `page` holding `credentials`, which hands each one it creates to `created`. */
async function authenticator(context: BrowserContext, page: Page, credentials: any[], created: (credential: any) => void) {
	const cdp = await context.newCDPSession(page);
	await cdp.send('WebAuthn.enable');
	const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', {
		options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true }
	});
	for (const credential of credentials) await cdp.send('WebAuthn.addCredential', { authenticatorId, credential });
	cdp.on('WebAuthn.credentialAdded', ({ credential }) => created(credential));
	// A sign-in raises the credential's counter, which the server checks: keep the latest.
	cdp.on('WebAuthn.credentialAsserted', ({ credential }) => created(credential));
}

const signInCard = (page: Page) => page.getByRole('form', { name: 'Sign in' });

/** The app at its own origin, on the connect screen for the server at localhost. */
async function connectToLocalhost(page: Page): Promise<void> {
	await page.goto(`/#server=${encodeURIComponent(server)}`);
	await signInCard(page).getByRole('button', { name: 'Continue', exact: true }).click();
	await expect(signInCard(page).getByText(server)).toBeVisible();
}

test('the web app, where it cannot use the RP ID, creates an account and signs in on the passkey page', async ({ page, context, request }) => {
	// Web clients from before the passkey page have no /signed-in/ to come back to.
	test.skip((await request.get('/signed-in/')).status() === 404, 'the web client under test has no passkey page support (APRON_WEB_DIR)');
	const credentials = new Map<string, any>();
	const keep = (credential: any) => credentials.set(credential.credentialId, credential);
	await authenticator(context, page, [], keep);
	context.on('page', (popup) => void authenticator(context, popup, [...credentials.values()], keep));

	// The guest creates an account: the begin offers only the page, which opens in a popup and
	// adds the passkey to the guest, who keeps their user ID.
	await connectToLocalhost(page);
	const card = signInCard(page);
	await card.getByRole('radiogroup', { name: 'Passkey', exact: true }).getByRole('radio', { name: /^Create account/ }).click();
	await card.getByLabel('Display name').fill('Lin');
	const opened = page.waitForEvent('popup');
	await card.getByRole('button', { name: 'Create account with passkey', exact: true }).click();
	const popup = await opened;
	await expect(popup.getByRole('heading', { name: 'Add a passkey' })).toBeVisible();
	await expect(popup.getByText(/^An app on this device wants to add a passkey to Lin \(@guest_\d+\)’s account on localhost:8080\.$/)).toBeVisible();
	const closed = popup.waitForEvent('close');
	await popup.getByRole('button', { name: 'Create passkey' }).click();
	await closed;
	await expect(card).toHaveCount(0);
	await expect(page.getByTestId('connection-status')).toHaveText('Connected');
	expect(credentials.size).toBe(1);

	// Another browser, whose popups are blocked: the app goes to the page in its own tab, signs in
	// there, and finishes on the way back.
	const other = await context.browser()!.newContext();
	const tab = await other.newPage();
	await tab.addInitScript(() => {
		window.open = () => null;
	});
	await authenticator(other, tab, [...credentials.values()], keep);
	await connectToLocalhost(tab);
	await signInCard(tab).getByRole('button', { name: 'Sign in with passkey', exact: true }).click();
	await tab.waitForURL(/^http:\/\/localhost:8080\/passkey\?action=login&/);
	await expect(tab.getByRole('heading', { name: `Sign in to ${SERVER_TITLE} on an app on this device?` })).toBeVisible();
	await tab.getByRole('button', { name: 'Continue with passkey' }).click();
	await tab.waitForURL((url) => url.origin === 'http://127.0.0.1:5173' && url.pathname === '/');
	await expect(tab.getByTestId('connection-status')).toHaveText('Connected');
	await expect(tab.getByRole('button', { name: /^Open preferences\..*: Lin(,|$)/ })).toBeVisible();
	await other.close();
});
