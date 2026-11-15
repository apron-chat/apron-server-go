import { createHash, randomBytes } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';

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
	const url = new URL((await back).url());
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
	await expect(page.getByText('app.example wants to sign in to your account on localhost:8080. It will be able to read and send messages as you.')).toBeVisible();
	await expect(page.getByText('Only continue if you just started signing in on app.example.')).toBeVisible();
	await page.getByRole('button', { name: 'Continue with passkey' }).click();
	await expect(page.getByText(`Allow app.example to use Grace (@${you.user_id})’s account?`)).toBeVisible();
	await expect(page.getByRole('button', { name: 'Not you? Use another passkey' })).toBeVisible();
	code = await returned(page, request.state, () => page.getByRole('button', { name: 'Allow' }).click());
	const signedIn = await app.request('auth', {
		scheme: 'webauthn', action: 'login', step: 'finish', code, code_verifier: request.verifier, redirect_uri: redirectURI, client_id: clientID
	});
	expect(signedIn.you.user_id).toBe(you.user_id);
	app.close();
});
