/**
 * Web push (§4.7) and status (§4.11) end to end between the web client and this server.
 * Runs with its own config, which starts aprond with a fixed VAPID key and --push.allow-insecure:
 *   npx playwright test --config=push.config.ts
 * See push.config.ts.
 */
import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import type { AddressInfo } from 'node:net';
import { expect, test, type BrowserContext, type Page, type WebSocketRoute } from '@playwright/test';
import { VAPID_PRIVATE_KEY, VAPID_SUBJECT } from './push.config';
import { openChat, userIdOf } from './test-helpers';

const VAPID_PRIVATE = VAPID_PRIVATE_KEY;
const PROXIED_WS = 'ws://127.0.0.1:5173/ws';
const ORIGIN = 'http://localhost:5173';
/** Set APRON_PUSH_EVIDENCE to a file to record the frames, headers, and payloads checked, as JSON lines. */
const EVIDENCE = process.env.APRON_PUSH_EVIDENCE;
const evidence = (label: string, data: unknown) => { if (EVIDENCE) fs.appendFileSync(EVIDENCE, JSON.stringify({ label, data }) + '\n'); };

type Frame = Record<string, any>;
const b64u = (buffer: Buffer | Uint8Array) => Buffer.from(buffer).toString('base64url');

// ---------- Push capture endpoint ----------
interface Captured { url: string; headers: http.IncomingHttpHeaders; body: Buffer; at: number }
async function captureServer() {
	const posts: Captured[] = [];
	const server = http.createServer((req, res) => {
		const chunks: Buffer[] = [];
		req.on('data', (chunk) => chunks.push(chunk));
		req.on('end', () => {
			posts.push({ url: req.url!, headers: req.headers, body: Buffer.concat(chunks), at: Date.now() });
			res.writeHead(201).end();
		});
	});
	await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
	const port = (server.address() as AddressInfo).port;
	return { posts, origin: `http://127.0.0.1:${port}`, close: () => server.close() };
}

// ---------- RFC 8291 decryption with our own subscription keys ----------
function hmac(key: Buffer, data: Buffer) { return crypto.createHmac('sha256', key).update(data).digest(); }
function decrypt(body: Buffer, ua: crypto.ECDH, authSecret: Buffer) {
	const salt = body.subarray(0, 16);
	const rs = body.readUInt32BE(16);
	const idlen = body[20];
	const asPublic = body.subarray(21, 21 + idlen);
	const ciphertext = body.subarray(21 + idlen);
	const uaPublic = ua.getPublicKey();
	const ecdhSecret = ua.computeSecret(asPublic);
	const prkKey = hmac(authSecret, ecdhSecret);
	const ikm = hmac(prkKey, Buffer.concat([Buffer.from('WebPush: info\0'), uaPublic, asPublic, Buffer.from([1])]));
	const prk = hmac(salt, ikm);
	const cek = hmac(prk, Buffer.from('Content-Encoding: aes128gcm\0\x01', 'binary')).subarray(0, 16);
	const nonce = hmac(prk, Buffer.from('Content-Encoding: nonce\0\x01', 'binary')).subarray(0, 12);
	const decipher = crypto.createDecipheriv('aes-128-gcm', cek, nonce);
	decipher.setAuthTag(ciphertext.subarray(ciphertext.length - 16));
	const padded = Buffer.concat([decipher.update(ciphertext.subarray(0, ciphertext.length - 16)), decipher.final()]);
	let end = padded.length - 1;
	while (end >= 0 && padded[end] === 0) end--;
	expect(padded[end], 'last-record delimiter 0x02').toBe(2);
	return { rs, idlen, asPublic: b64u(asPublic), plaintext: padded.subarray(0, end).toString('utf8') };
}

function vapidPublicFromPrivate(privateKey: string): string {
	const ecdh = crypto.createECDH('prime256v1');
	ecdh.setPrivateKey(Buffer.from(privateKey, 'base64url'));
	return b64u(ecdh.getPublicKey());
}

function verifyVapid(authorization: string | undefined, audience: string) {
	const match = /^vapid t=([^,\s]+),\s*k=([A-Za-z0-9_-]+)$/.exec(authorization ?? '');
	expect(match, `Authorization: ${authorization}`).not.toBeNull();
	const [, token, k] = match!;
	const [h, p, s] = token.split('.');
	const header = JSON.parse(Buffer.from(h, 'base64url').toString());
	const claims = JSON.parse(Buffer.from(p, 'base64url').toString());
	const point = Buffer.from(k, 'base64url');
	const key = crypto.createPublicKey({ key: { kty: 'EC', crv: 'P-256', x: b64u(point.subarray(1, 33)), y: b64u(point.subarray(33, 65)) }, format: 'jwk' });
	const valid = crypto.verify('sha256', Buffer.from(`${h}.${p}`), { key, dsaEncoding: 'ieee-p1363' }, Buffer.from(s, 'base64url'));
	return { header, claims, k, valid, lifetime: claims.exp - Math.floor(Date.now() / 1000) };
}

// ---------- Raw protocol client (the second user / observer) ----------
class Raw {
	frames: Frame[] = [];
	private waiting = new Map<string, (frame: Frame) => void>();
	private next = 0;
	userId = '';
	constructor(private socket: WebSocket) {
		socket.addEventListener('message', (event) => {
			const frame = JSON.parse(String(event.data)) as Frame;
			this.frames.push(frame);
			if (typeof frame.id === 'string') this.waiting.get(frame.id)?.(frame);
		});
	}
	static async connect(name: string): Promise<Raw> {
		const socket = new WebSocket(PROXIED_WS);
		await new Promise((resolve, reject) => {
			socket.addEventListener('open', resolve, { once: true });
			socket.addEventListener('error', reject, { once: true });
		});
		const raw = new Raw(socket);
		const auth = await raw.request('auth', { scheme: 'guest', name });
		raw.userId = auth.you.user_id;
		return raw;
	}
	request(method: string, params: Record<string, unknown>): Promise<Frame> {
		return new Promise((resolve, reject) => {
			const id = `raw-${this.next++}`;
			this.waiting.set(id, (frame) => (frame.error ? reject(new Error(JSON.stringify(frame.error))) : resolve(frame.result)));
			this.socket.send(JSON.stringify({ method, id, params }));
		});
	}
	notify(method: string, params: Record<string, unknown>) { this.socket.send(JSON.stringify({ method, params })); }
	close() { this.socket.close(); }
}

// ---------- Page WebSocket tap ----------
interface Tap { sent: Frame[]; received: Frame[]; server?: WebSocketRoute; inject(frame: Frame): void }
async function tapSocket(page: Page): Promise<Tap> {
	const tap: Tap = { sent: [], received: [], inject(frame) { tap.server!.send(JSON.stringify(frame)); } };
	await page.routeWebSocket('**/ws', (ws) => {
		const server = ws.connectToServer();
		tap.server = server;
		ws.onMessage((message) => {
			try { tap.sent.push(JSON.parse(String(message))); } catch { /* binary */ }
			server.send(message);
		});
		server.onMessage((message) => {
			try { tap.received.push(JSON.parse(String(message))); } catch { /* binary */ }
			ws.send(message);
		});
	});
	return tap;
}

// ---------- Passkey account ----------
/** A virtual authenticator on the page, with the given credentials. */
async function addAuthenticator(page: Page, context: BrowserContext, credentials: any[] = []) {
	const cdp = await context.newCDPSession(page);
	await cdp.send('WebAuthn.enable');
	const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', { options: {
		protocol: 'ctap2', transport: 'internal', hasResidentKey: true,
		hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true
	} });
	for (const credential of credentials) await cdp.send('WebAuthn.addCredential', { authenticatorId, credential });
	return { credentials: async () => (await cdp.send('WebAuthn.getCredentials', { authenticatorId })).credentials };
}

async function signUpWithPasskey(page: Page, context: BrowserContext): Promise<{ credentials: () => Promise<any[]> }> {
	const authenticator = await addAuthenticator(page, context);
	await page.getByRole('button', { name: /^Your profile on/ }).click();
	await page.getByRole('dialog', { name: 'Edit profile' }).getByRole('button', { name: 'Sign in with a passkey', exact: true }).click();
	const card = page.getByRole('form', { name: 'Sign in' });
	await card.getByRole('radiogroup', { name: 'Passkey', exact: true }).getByRole('radio', { name: /^Create account/ }).click();
	await card.getByRole('button', { name: 'Create account with passkey', exact: true }).click();
	await expect(card).toHaveCount(0);
	return authenticator;
}

async function signInWithPasskey(page: Page): Promise<void> {
	// Signing out leaves the profile editor open.
	if (!(await page.getByRole('dialog', { name: 'Edit profile' }).count())) await page.getByRole('button', { name: /^Your profile on/ }).click();
	await page.getByRole('dialog', { name: 'Edit profile' }).getByRole('button', { name: 'Sign in with a passkey', exact: true }).click();
	const card = page.getByRole('form', { name: 'Sign in' });
	await card.getByRole('button', { name: 'Sign in with passkey', exact: true }).click();
	await expect(card).toHaveCount(0);
}

/** A PushManager whose subscription points at the capture server, with keys this test holds. */
function pushStub(endpoint: string, p256dh: string, auth: string) {
	return ({ endpoint, p256dh, auth }: { endpoint: string; p256dh: string; auth: string }) => {
		const scope = globalThis as any;
		if (!scope.PushManager) return;
		const KEY = '__apron_test_push_key';
		scope.__pushLog = scope.__pushLog ?? [];
		let memory: string | null = null;
		const store = {
			get: () => { try { return scope.localStorage ? scope.localStorage.getItem(KEY) : memory; } catch { return memory; } },
			set: (value: string | null) => { try { if (scope.localStorage) { if (value === null) scope.localStorage.removeItem(KEY); else scope.localStorage.setItem(KEY, value); } else memory = value; } catch { memory = value; } }
		};
		const toBytes = (b64: string) => Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
		const make = (keyB64: string) => ({
			endpoint, expirationTime: null,
			options: { userVisibleOnly: true, applicationServerKey: toBytes(keyB64).buffer },
			getKey: (name: string) => toBytes((name === 'auth' ? auth : p256dh).replace(/-/g, '+').replace(/_/g, '/').padEnd(Math.ceil((name === 'auth' ? auth : p256dh).length / 4) * 4, '=')).buffer,
			toJSON: () => ({ endpoint, expirationTime: null, keys: { p256dh, auth } }),
			unsubscribe: async () => { scope.__pushLog.push('unsubscribe'); store.set(null); return true; }
		});
		scope.PushManager.prototype.subscribe = async function (options: { applicationServerKey: ArrayBuffer | Uint8Array }) {
			const bytes = new Uint8Array(options.applicationServerKey as ArrayBuffer);
			const keyB64 = btoa(String.fromCharCode(...bytes));
			scope.__pushLog.push('subscribe');
			store.set(keyB64);
			return make(keyB64);
		};
		scope.PushManager.prototype.getSubscription = async function () {
			const keyB64 = store.get();
			return keyB64 ? make(keyB64) : null;
		};
		scope.PushManager.prototype.permissionState = async () => 'granted';
	};
}

const findSent = (tap: Tap, method: string) => tap.sent.filter((frame) => frame.method === method);
const resultOf = (tap: Tap, id: string) => tap.received.find((frame) => frame.id === id);

async function waitPosts(posts: Captured[], count: number, timeout = 10_000) {
	await expect.poll(() => posts.length, { timeout }).toBeGreaterThanOrEqual(count);
}
/** No push arrives in a quiet window, counted from `before` (taken before the triggering action). */
async function expectNoPost(posts: Captured[], label: string, before: number, ms = 3_000) {
	await new Promise((resolve) => setTimeout(resolve, ms));
	expect(posts.length, `no push expected: ${label}`).toBe(before);
	evidence(`no-push:${label}`, { posts: posts.length });
}

async function goIdle(page: Page, tap: Tap) {
	const before = findSent(tap, 'status').length;
	const started = Date.now();
	await page.evaluate(() => window.dispatchEvent(new Event('blur')));
	await expect.poll(() => findSent(tap, 'status').slice(before).find((frame) => frame.params?.idle === true), { timeout: 45_000, intervals: [500] }).toBeTruthy();
	return Date.now() - started;
}
async function goAttended(page: Page, tap: Tap) {
	const before = findSent(tap, 'status').length;
	await page.evaluate(() => window.dispatchEvent(new Event('focus')));
	await expect.poll(() => findSent(tap, 'status').slice(before).find((frame) => frame.params?.idle === false)).toBeTruthy();
}

async function swNotifications(context: BrowserContext) {
	const [sw] = context.serviceWorkers();
	return sw.evaluate(async () => (await (self as any).registration.getNotifications()).map((n: Notification) => ({ title: n.title, body: n.body, tag: n.tag, data: n.data })));
}


test('web push: register, wake rules, VAPID + aes128gcm delivery, service worker notification', async ({ page, context }) => {
	const capture = await captureServer();
	const ua = crypto.createECDH('prime256v1');
	ua.generateKeys();
	const authSecret = crypto.randomBytes(16);
	const endpoint = `${capture.origin}/push/sub-${Date.now()}`;
	const p256dh = b64u(ua.getPublicKey());
	const auth = b64u(authSecret);
	await context.grantPermissions(['notifications'], { origin: ORIGIN });
	await context.addInitScript(pushStub(endpoint, p256dh, auth), { endpoint, p256dh, auth });
	const tap = await tapSocket(page);
	const swPromise = context.waitForEvent('serviceworker');

	await openChat(page);
	const serverFrame = tap.received.find((frame) => frame.method === 'server');
	evidence('server.push', serverFrame?.params?.push);
	expect(serverFrame?.params?.push?.webpush?.key).toBe(vapidPublicFromPrivate(VAPID_PRIVATE));
	const authenticator = await signUpWithPasskey(page, context);
	const user1 = await userIdOf(page);
	evidence('user1', user1);

	// Stub PushManager in the service worker too.
	const sw = context.serviceWorkers()[0] ?? await swPromise;
	await sw.evaluate(pushStub(endpoint, p256dh, auth), { endpoint, p256dh, auth });

	// --- Turn on push in Preferences ---
	await page.getByRole('button', { name: /^Open preferences/ }).click();
	const prefs = page.getByRole('dialog', { name: 'Preferences' });
	const pushSwitch = prefs.getByRole('switch', { name: 'Push notifications' });
	await expect(prefs.getByRole('heading', { name: 'Preferences' })).toBeVisible();
	test.skip(await pushSwitch.count() === 0, 'This apron-web has no web push setting yet (apron-web#48)');
	await expect(pushSwitch).toHaveAttribute('aria-checked', 'false');
	await pushSwitch.click();
	await expect(pushSwitch).toHaveAttribute('aria-checked', 'true');
	await expect.poll(() => findSent(tap, 'push_register').length).toBe(1);
	const register = findSent(tap, 'push_register')[0];
	await expect.poll(() => resultOf(tap, register.id)).toBeTruthy();
	evidence('push_register', { frame: register, answer: resultOf(tap, register.id) });
	expect(resultOf(tap, register.id)!.error).toBeUndefined();
	expect(register.params.kind).toBe('webpush');
	expect(register.params.url).toBe(endpoint);
	expect(register.params.keys).toEqual({ p256dh, auth });
	expect(register.params.push_id).toMatch(/^[A-Za-z0-9_-]{1,64}$/);
	expect(register.params.wake).toEqual(['mentions', 'replies']);
	const pushId = register.params.push_id as string;
	// Opaque (§4.7): it names neither the server nor the account.
	expect(pushId).not.toContain(user1);
	await page.keyboard.press('Escape');

	// --- Idle (real client path: window blur, idle after ~30s) ---
	const idleAfter = await goIdle(page, tap);
	evidence('idle sent after ms', idleAfter);
	expect(idleAfter).toBeGreaterThan(25_000);

	const user2 = await Raw.connect('Second');
	evidence('user2', user2.userId);

	// --- Mention ---
	const mention = await user2.request('message', { room_id: 'general', body: { text: `hey @${user1} ping`, mentions: [user1] } });
	await waitPosts(capture.posts, 1);
	const post = capture.posts[0];
	const vapid = verifyVapid(post.headers.authorization, capture.origin);
	const decoded = decrypt(post.body, ua, authSecret);
	const payload = JSON.parse(decoded.plaintext);
	evidence('mention push', { url: post.url, headers: post.headers, vapid, rs: decoded.rs, bytes: decoded.plaintext.length, payload });
	expect(post.url).toBe(new URL(endpoint).pathname);
	expect(vapid.valid).toBe(true);
	expect(vapid.header).toMatchObject({ alg: 'ES256' });
	expect(vapid.claims.aud).toBe(capture.origin);
	expect(vapid.lifetime).toBeLessThanOrEqual(24 * 3600);
	expect(vapid.claims.sub).toBe(VAPID_SUBJECT);
	expect(vapid.k).toBe(vapidPublicFromPrivate(VAPID_PRIVATE));
	expect(Number(post.headers.ttl)).toBeGreaterThan(0);
	expect(post.headers.urgency).toBe('high');
	expect(post.headers['content-encoding']).toBe('aes128gcm');
	expect(Buffer.byteLength(decoded.plaintext)).toBeLessThanOrEqual(2048);
	expect(payload.push_id).toBe(pushId);
	expect(payload.message.message_id).toBe(mention.message_id);
	expect(payload.message).not.toHaveProperty('log_id');
	expect(payload.message.room_id).toBe('general');
	expect(payload.message.from.user_id).toBe(user2.userId);
	expect(typeof payload.unread).toBe('number');

	// --- Deliver to the real service worker ---
	const cdp = await context.newCDPSession(page);
	const registrations: any[] = [];
	cdp.on('ServiceWorker.workerRegistrationUpdated', (event) => registrations.push(...event.registrations));
	await cdp.send('ServiceWorker.enable');
	await expect.poll(() => registrations.find((r) => !r.isDeleted)).toBeTruthy();
	const registrationId = registrations.find((r) => !r.isDeleted).registrationId;
	await cdp.send('ServiceWorker.deliverPushMessage', { origin: ORIGIN, registrationId, data: decoded.plaintext });
	const tag = `apron:${pushId}:${mention.message_id}`;
	await expect.poll(async () => (await swNotifications(context)).filter((n: any) => n.tag === tag).length).toBe(1);
	const shown = await swNotifications(context);
	evidence('notifications after first delivery', shown);
	const mine = shown.find((n: any) => n.tag === tag);
	expect(mine.title).toBe(`Second · general`);
	expect(mine.body).toContain('ping');
	await cdp.send('ServiceWorker.deliverPushMessage', { origin: ORIGIN, registrationId, data: decoded.plaintext });
	await new Promise((resolve) => setTimeout(resolve, 1_000));
	const again = await swNotifications(context);
	evidence('notifications after duplicate delivery', again);
	expect(again.filter((n: any) => n.tag === tag)).toHaveLength(1);
	expect(again).toHaveLength(shown.length);

	// Every later push is matched by message_id, not by arrival order.
	const decryptAll = () => capture.posts.map((p) => ({ post: p, payload: JSON.parse(decrypt(p.body, ua, authSecret).plaintext) }));
	const pushOf = async (messageId: string) => {
		await expect.poll(() => decryptAll().some((entry) => entry.payload.message?.message_id === messageId), { timeout: 10_000 }).toBe(true);
		return decryptAll().find((entry) => entry.payload.message?.message_id === messageId)!;
	};
	// user2 says `status`, so it sees user1's status change once the server has applied it.
	user2.notify('status', { idle: false });
	const seenStatus = () => user2.frames.filter((f) => f.method === 'user' && f.params?.new?.user_id === user1).map((f) => f.params.new.status).at(-1);

	// --- User 1's own message: no push. Sent through the UI while idle. ---
	const statusBefore = findSent(tap, 'status').length;
	let before = capture.posts.length;
	const own = `own-${Date.now()}`;
	await page.getByRole('textbox', { name: 'Message', exact: true }).fill(own);
	await page.getByRole('button', { name: 'Send message', exact: true }).click();
	await expect.poll(() => findSent(tap, 'message').find((frame) => frame.params?.body?.text === own)).toBeTruthy();
	const ownFrame = findSent(tap, 'message').find((frame) => frame.params?.body?.text === own)!;
	await expect.poll(() => resultOf(tap, ownFrame.id)?.result?.message_id).toBeTruthy();
	const ownId = resultOf(tap, ownFrame.id)!.result.message_id as string;
	evidence('status frames sent while composing', findSent(tap, 'status').slice(statusBefore));
	expect(findSent(tap, 'status').slice(statusBefore).some((frame) => frame.params?.idle === false), 'sending did not end idle').toBe(false);
	await expectNoPost(capture.posts, 'own message', before);

	// --- Plain message in a joined room: no push by default ---
	before = capture.posts.length;
	await user2.request('message', { room_id: 'general', body: { text: 'plain chatter' } });
	await expectNoPost(capture.posts, 'plain message in joined room', before);

	// --- Reply: push (wake replies) ---
	const reply = await user2.request('message', { room_id: 'general', body: { text: 'a reply to you' }, reply_to: { message_id: ownId } });
	const replyPush = await pushOf(reply.message_id);
	evidence('reply push', { urgency: replyPush.post.headers.urgency, ttl: replyPush.post.headers.ttl, payload: replyPush.payload });
	expect(replyPush.payload.message.reply_to?.message_id).toBe(ownId);
	expect(replyPush.post.headers.urgency).toBe('high');

	// --- Unscoped mute through the UI pause menu ---
	await page.getByRole('button', { name: /^Open preferences/ }).click();
	await prefs.getByRole('button', { name: /^Pause/ }).click();
	await page.getByRole('menuitem', { name: /Until I resume/ }).or(page.getByRole('option', { name: /Until I resume/ })).first().click();
	await expect.poll(() => findSent(tap, 'status').find((frame) => frame.params?.mute === true)).toBeTruthy();
	await expect.poll(() => tap.received.find((frame) => frame.method === 'user' && frame.params?.you?.mute === true)).toBeTruthy();
	evidence('mute frame sent', findSent(tap, 'status').find((frame) => frame.params?.mute === true));
	evidence('mute echo', tap.received.filter((frame) => frame.method === 'user' && frame.params?.you?.mute !== undefined));
	await page.keyboard.press('Escape');
	before = capture.posts.length;
	await user2.request('message', { room_id: 'general', body: { text: `muted mention @${user1}`, mentions: [user1] } });
	await user2.request('message', { room_id: 'general', body: { text: 'muted reply' }, reply_to: { message_id: ownId } });
	await expectNoPost(capture.posts, 'mention+reply while unscoped mute (webpush gets no badge either)', before, 4_000);
	await page.getByRole('button', { name: /^Open preferences/ }).click();
	await prefs.getByRole('button', { name: 'Resume', exact: true }).click();
	await expect.poll(() => findSent(tap, 'status').find((frame) => frame.params?.mute === 0)).toBeTruthy();
	await expect.poll(() => tap.received.find((frame) => frame.method === 'user' && frame.params?.you?.mute === 0)).toBeTruthy();
	await page.keyboard.press('Escape');

	// --- Room mute via raw status: mention pushes, a reply doesn't ---
	tap.inject({ method: 'status', params: { room_id: 'general', mute: true } });
	await expect.poll(() => tap.received.find((frame) => frame.method === 'room_update' && JSON.stringify(frame.params).includes('"mute":true'))).toBeTruthy();
	evidence('room mute echo', tap.received.filter((frame) => frame.method === 'room_update' && JSON.stringify(frame.params).includes('"mute"')).map((f) => f.params));
	before = capture.posts.length;
	await user2.request('message', { room_id: 'general', body: { text: 'reply in muted room' }, reply_to: { message_id: ownId } });
	await expectNoPost(capture.posts, 'reply in muted room', before);
	const mutedMention = await user2.request('message', { room_id: 'general', body: { text: `mention in muted room @${user1}`, mentions: [user1] } });
	const mutedPush = await pushOf(mutedMention.message_id);
	evidence('room-muted mention push', { urgency: mutedPush.post.headers.urgency, payload: mutedPush.payload });
	tap.inject({ method: 'status', params: { room_id: 'general', mute: 0 } });
	await expect.poll(() => tap.received.find((frame) => frame.method === 'room_update' && JSON.stringify(frame.params).includes('"mute":0'))).toBeTruthy();

	// --- Attended: no push ---
	await goAttended(page, tap);
	await expect.poll(seenStatus).toBe('online');
	before = capture.posts.length;
	await user2.request('message', { room_id: 'general', body: { text: `attended mention @${user1}`, mentions: [user1] } });
	await expectNoPost(capture.posts, 'mention while attended', before);

	// --- Tab closed: push ---
	const passkeys = await authenticator.credentials();
	await page.close();
	await expect.poll(seenStatus).toBe('idle');
	const closedMention = await user2.request('message', { room_id: 'general', body: { text: `closed mention @${user1}`, mentions: [user1] } });
	const closedPush = await pushOf(closedMention.message_id);
	evidence('closed-tab mention push', { payload: closedPush.payload, statusSeenByUser2: user2.frames.filter((f) => f.method === 'user' && f.params?.new?.user_id === user1).map((f) => f.params.new.status) });

	// --- Reopen: the session resumes and registers again, with the same push_id ---
	const page2 = await context.newPage();
	const tap2 = await tapSocket(page2);
	await openChat(page2);
	await addAuthenticator(page2, context, passkeys);
	await expect.poll(() => findSent(tap2, 'push_register').length).toBeGreaterThanOrEqual(1);
	const again2 = findSent(tap2, 'push_register')[0];
	evidence('re-register on connect', again2);
	expect(again2.params).toEqual(register.params);
	const enabledIds = () => page2.evaluate(() => new Promise<unknown>((resolve) => {
		const request = indexedDB.open('apron-push');
		request.onsuccess = () => {
			const get = request.result.transaction('state').objectStore('state').get('enabled');
			get.onsuccess = () => { resolve(get.result); request.result.close(); };
			get.onerror = () => resolve('error');
		};
		request.onerror = () => resolve('error');
	}));
	expect(await enabledIds()).toContain(pushId);

	// --- Sign out: unregister, push off for this account here, and this device stops showing it ---
	await page2.getByRole('button', { name: /^Your profile on/ }).click();
	await page2.getByRole('dialog', { name: 'Edit profile' }).getByRole('button', { name: 'Sign out', exact: true }).click();
	await expect.poll(() => findSent(tap2, 'push_unregister').length).toBeGreaterThanOrEqual(1);
	const unregister = findSent(tap2, 'push_unregister')[0];
	evidence('push_unregister', { frame: unregister, answer: resultOf(tap2, unregister.id) ?? 'connection replaced before answer' });
	expect(unregister.params).toEqual({ url: endpoint });
	await expect.poll(seenStatus).toBe('offline');
	before = capture.posts.length;
	await user2.request('message', { room_id: 'general', body: { text: `after signout @${user1}`, mentions: [user1] } });
	await expectNoPost(capture.posts, 'mention after sign-out', before);
	await expect.poll(enabledIds).not.toContain(pushId);
	evidence('enabled push_ids after sign-out', await enabledIds());
	// A late push for the signed-out account is dropped: nothing new shows.
	const shownBefore = await swNotifications(context);
	const late = JSON.stringify({ push_id: pushId, unread: 1, message: { message_id: '9999999999999', room_id: 'general', from: { user_id: user2.userId, name: 'Second' }, body: { text: 'late push' } } });
	// The first page's CDP session closed with it: find the registration again from this page.
	const cdp2 = await context.newCDPSession(page2);
	const registrations2: any[] = [];
	cdp2.on('ServiceWorker.workerRegistrationUpdated', (event) => registrations2.push(...event.registrations));
	await cdp2.send('ServiceWorker.enable');
	await expect.poll(() => registrations2.find((r) => !r.isDeleted)).toBeTruthy();
	await cdp2.send('ServiceWorker.deliverPushMessage', { origin: ORIGIN, registrationId: registrations2.find((r) => !r.isDeleted).registrationId, data: late });
	await new Promise((resolve) => setTimeout(resolve, 1_000));
	const shownAfter = await swNotifications(context);
	evidence('notifications after a push for the signed-out account', { before: shownBefore, after: shownAfter });
	expect(shownAfter.filter((n: any) => n.body === 'late push')).toHaveLength(0);
	expect(shownAfter).toHaveLength(shownBefore.length);

	// --- Signing in again: push stays off, and the client unregisters this browser's endpoint ---
	const sentBefore = tap2.sent.length;
	await signInWithPasskey(page2);
	expect(await userIdOf(page2)).toBe(user1);
	await expect.poll(() => tap2.sent.slice(sentBefore).filter((frame) => frame.method === 'push_unregister').length).toBeGreaterThanOrEqual(1);
	const afterSignIn = tap2.sent.slice(sentBefore).filter((frame) => frame.method === 'push_register' || frame.method === 'push_unregister');
	evidence('push frames after signing in again', afterSignIn);
	expect(afterSignIn.every((frame) => frame.method === 'push_unregister' && frame.params.url === endpoint)).toBe(true);
	await page2.getByRole('button', { name: /^Open preferences/ }).click();
	await expect(page2.getByRole('dialog', { name: 'Preferences' }).getByRole('switch', { name: 'Push notifications' })).toHaveAttribute('aria-checked', 'false');
	await page2.keyboard.press('Escape');
	evidence('total posts', capture.posts.length);
	user2.close();
	capture.close();
});

test('status: online, idle, dnd, offline as another client sees them; mute and invisible stay private', async ({ page, context, browser }) => {
	const tap = await tapSocket(page);
	await openChat(page);
	// A client implementing §4.11 reports its initial idle at once.
	test.skip(!findSent(tap, 'status').length, 'This apron-web does not send status yet (apron-web#48)');
	await signUpWithPasskey(page, context);
	const user1 = await userIdOf(page);

	// Observer 1: a raw status-aware client. Observer 2: the web client as a guest in another context.
	const observer = await Raw.connect('Observer');
	observer.notify('status', { idle: false });
	const otherContext = await browser.newContext({ baseURL: ORIGIN });
	const otherPage = await otherContext.newPage();
	const otherTap = await tapSocket(otherPage);
	await openChat(otherPage);
	await new Promise((resolve) => setTimeout(resolve, 1_000));

	const statusesSeen = (frames: Frame[]) => frames.filter((frame) => frame.method === 'user' && frame.params?.new?.user_id === user1).map((frame) => frame.params.new.status);
	const lastSeen = (frames: Frame[]) => statusesSeen(frames).at(-1);
	const expectBoth = async (status: string) => {
		await expect.poll(() => lastSeen(observer.frames), { timeout: 15_000 }).toBe(status);
		await expect.poll(() => lastSeen(otherTap.received), { timeout: 15_000 }).toBe(status);
	};
	evidence('other web client sent status', findSent(otherTap, 'status'));

	await goIdle(page, tap);
	await expectBoth('idle');
	await goAttended(page, tap);
	await expectBoth('online');

	await page.getByRole('button', { name: /^Open preferences/ }).click();
	const prefs = page.getByRole('dialog', { name: 'Preferences' });
	await prefs.getByRole('button', { name: /^Pause/ }).click();
	await page.getByRole('menuitem', { name: /For 1 hour/ }).or(page.getByRole('option', { name: /For 1 hour/ })).first().click();
	await expectBoth('dnd');
	const youMute = tap.received.filter((frame) => frame.method === 'user' && frame.params?.you).map((frame) => frame.params.you);
	evidence('user1 own you frames after mute', youMute);
	await prefs.getByRole('button', { name: 'Resume', exact: true }).click();
	await expectBoth('online');
	await page.keyboard.press('Escape');

	tap.inject({ method: 'status', params: { invisible: true } });
	await expectBoth('offline');
	await expect.poll(() => tap.received.find((frame) => frame.method === 'user' && frame.params?.you?.invisible === true)).toBeTruthy();
	evidence('user1 you while invisible', tap.received.filter((frame) => frame.method === 'user' && frame.params?.you?.invisible === true).map((frame) => frame.params.you));
	// Idle while invisible: others still see offline (no new transition).
	tap.inject({ method: 'status', params: { invisible: false } });
	await expectBoth('online');
	tap.inject({ method: 'status', params: { room_id: 'general', mute: true } });
	await new Promise((resolve) => setTimeout(resolve, 1_500));
	tap.inject({ method: 'status', params: { room_id: 'general', mute: 0 } });

	await page.close();
	await expectBoth('offline');

	evidence('observer statuses for user1', statusesSeen(observer.frames));
	evidence('web observer statuses for user1', statusesSeen(otherTap.received));
	// No frame to others ever names user1's mute or invisible, or a room mute of theirs.
	const leaks = (frames: Frame[]) => {
		const found: unknown[] = [];
		const walk = (value: any, frame: Frame) => {
			if (!value || typeof value !== 'object') return;
			if (value.user_id === user1 && ('mute' in value || 'invisible' in value)) found.push(frame);
			for (const child of Object.values(value)) walk(child, frame);
		};
		for (const frame of frames) walk(frame, frame);
		const roomMutes = frames.filter((frame) => frame.method === 'room_update' && JSON.stringify(frame.params).includes('"mute"'));
		return [...found, ...roomMutes];
	};
	expect(leaks(observer.frames)).toEqual([]);
	expect(leaks(otherTap.received)).toEqual([]);
	observer.close();
	await otherContext.close();
});
