import { expect, test, type Page, type WebSocketRoute } from '@playwright/test';
import { editMessage, openChat, sendMessage, waitForMessage } from './test-helpers';

test.use({ baseURL: 'http://localhost:5173' });

const profileButton = (page: Page) => page.getByRole('button', { name: /^Your profile on/ });
const profileDialog = (page: Page) => page.getByRole('dialog', { name: 'Edit profile' });
const connectCard = (page: Page) => page.getByRole('form', { name: 'Sign in' });

/** The user_id the profile editor shows, leaving the editor as it found it. */
async function identityOf(page: Page): Promise<string> {
	const wasOpen = await profileDialog(page).count() > 0;
	if (!wasOpen) await profileButton(page).click();
	const identity = await profileDialog(page).locator('code').textContent();
	if (!wasOpen) await profileButton(page).click();
	return identity!;
}

/** Signing in lives on the connect screen; the profile's button opens it with Passkey chosen. */
async function openPasskeySignIn(page: Page): Promise<void> {
	if (!(await profileDialog(page).count())) await profileButton(page).click();
	await profileDialog(page).getByRole('button', { name: 'Sign in with a passkey', exact: true }).click();
	await expect(connectCard(page)).toBeVisible();
	await expect(connectCard(page).getByRole('radio', { name: 'Passkey', exact: true })).toHaveAttribute('aria-checked', 'true');
}

/**
 * The sign-in panel's two explicit passkey actions: on Passkey, the viewer picks Sign in (an
 * existing passkey's account, the default) or Create account (an account with a new passkey),
 * and the primary button does it.
 */
const chooseCreateAccount = (page: Page) => connectCard(page).getByRole('radiogroup', { name: 'Passkey', exact: true })
	.getByRole('radio', { name: /^Create account/ }).click();
const createAccount = (page: Page) => connectCard(page).getByRole('button', { name: 'Create account with passkey', exact: true });
const signInWithPasskey = (page: Page) => connectCard(page).getByRole('button', { name: 'Sign in with passkey', exact: true });

async function registerPasskey(page: Page): Promise<void> {
	await openPasskeySignIn(page);
	await chooseCreateAccount(page);
	await createAccount(page).click();
	await expect(connectCard(page)).toHaveCount(0);
}

async function loginWithPasskey(page: Page): Promise<void> {
	await openPasskeySignIn(page);
	await signInWithPasskey(page).click();
	await expect(connectCard(page)).toHaveCount(0);
}

async function signOut(page: Page): Promise<void> {
	if (!(await profileDialog(page).count())) await profileButton(page).click();
	await profileDialog(page).getByRole('button', { name: 'Sign out', exact: true }).click();
	await expect(page.getByTestId('connection-status')).toHaveText('Connected');
	await expect(profileDialog(page).getByRole('button', { name: 'Sign in with a passkey', exact: true })).toBeVisible();
	await profileButton(page).click();
}

test('passkeys preserve identity and edit ownership through sign-out, login, and reconnect', async ({ page, context }) => {
	let connection!: WebSocketRoute;
	let connections = 0;
	await page.routeWebSocket('**/ws', (route) => {
		route.connectToServer();
		connection = route;
		connections++;
	});
	const cdp = await context.newCDPSession(page);
	await cdp.send('WebAuthn.enable');
	const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', { options: {
		protocol: 'ctap2', transport: 'internal', hasResidentKey: true,
		hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true
	} });
	await openChat(page);
	const message = `passkey-${Date.now()}`;
	await sendMessage(page, message);
	const messageId = await (await waitForMessage(page, message)).getAttribute('data-message-id');
	const savedMessage = page.locator(`article[data-message-id="${messageId}"]`);
	const identity = await identityOf(page);
	// Creating an account registers a passkey for the current identity.
	await registerPasskey(page);
	const { credentials } = await cdp.send('WebAuthn.getCredentials', { authenticatorId });
	expect(credentials).toHaveLength(1);
	expect(credentials[0].isResidentCredential).toBe(true);
	expect(await identityOf(page)).toBe(identity);
	await signOut(page);
	expect(await identityOf(page)).not.toBe(identity);
	// Signing in with that passkey brings the identity back.
	await loginWithPasskey(page);
	expect(await identityOf(page)).toBe(identity);
	expect((await cdp.send('WebAuthn.getCredentials', { authenticatorId })).credentials).toHaveLength(1);
	await expect(page.getByLabel('Loading history', { exact: true })).toHaveCount(0);
	await editMessage(savedMessage, `${message}-edited`);
	await waitForMessage(page, `${message}-edited`);
	const beforeReconnect = connections;
	await connection.close({ code: 1012, reason: 'Test transport reconnection' });
	await expect.poll(() => connections).toBeGreaterThan(beforeReconnect);
	await expect(page.getByTestId('connection-status')).toHaveText('Connected', { timeout: 20_000 });
	expect(await identityOf(page)).toBe(identity);
	await expect(page.getByLabel('Loading history', { exact: true })).toHaveCount(0);
	await editMessage(savedMessage, `${message}-resumed`);
	await waitForMessage(page, `${message}-resumed`);
	// A reload resumes the persisted session token: the account survives without a ceremony.
	await page.reload();
	await expect(page.getByTestId('connection-status')).toHaveText('Connected');
	expect(await identityOf(page)).toBe(identity);
	await expect(page.getByLabel('Loading history', { exact: true })).toHaveCount(0);
	// Signing out forgets the stored token, so the next reload starts as a guest.
	await signOut(page);
	await page.reload();
	await expect(page.getByTestId('connection-status')).toHaveText('Connected');
	expect(await identityOf(page)).not.toBe(identity);
	await loginWithPasskey(page);
	expect(await identityOf(page)).toBe(identity);
});

test('a rejected passkey verification leaves the guest usable and allows retry', async ({ page, context }) => {
	const cdp = await context.newCDPSession(page);
	await cdp.send('WebAuthn.enable');
	const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', { options: {
		protocol: 'ctap2', transport: 'internal', hasResidentKey: true,
		hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true
	} });
	await openChat(page);
	const identity = await identityOf(page);
	await registerPasskey(page);
	await signOut(page);
	const guest = await identityOf(page);
	expect(guest).not.toBe(identity);
	await cdp.send('WebAuthn.setResponseOverrideBits', { authenticatorId, isBogusSignature: true });
	await openPasskeySignIn(page);
	const card = connectCard(page);
	await signInWithPasskey(page).click();
	await expect(card.getByRole('alert')).toContainText('Passkey verification failed');
	await card.getByRole('button', { name: 'Cancel', exact: true }).click();
	expect(await identityOf(page)).toBe(guest);
	await cdp.send('WebAuthn.setResponseOverrideBits', { authenticatorId, isBogusSignature: false });
	await loginWithPasskey(page);
	expect(await identityOf(page)).toBe(identity);
});

test('a handle typed in the profile names the new account and its passkey', async ({ page, context }) => {
	const cdp = await context.newCDPSession(page);
	await cdp.send('WebAuthn.enable');
	const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', { options: {
		protocol: 'ctap2', transport: 'internal', hasResidentKey: true,
		hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true
	} });
	await openChat(page);
	const handle = `handle-${Date.now()}`;
	await profileButton(page).click();
	await profileDialog(page).getByTestId('display-name-input').fill(handle);
	await profileDialog(page).getByRole('button', { name: 'Sign in with a passkey', exact: true }).click();
	// A new account takes a name; signing in restores one, so only Create account asks.
	await chooseCreateAccount(page);
	await expect(connectCard(page).getByTestId('connect-name-input')).toHaveValue(handle);
	await createAccount(page).click();
	await expect(connectCard(page)).toHaveCount(0);
	await expect(profileButton(page)).toContainText(handle);
	// The register begin carries the handle (§3.2 name): the passkey is saved under it, not the guest's user_id.
	const { credentials } = await cdp.send('WebAuthn.getCredentials', { authenticatorId });
	expect(credentials).toHaveLength(1);
	expect(credentials[0].userName).toBe(handle);
});

test('cancelling an active passkey prompt leaves sign-in and server changes usable', async ({ page, context }) => {
	const cdp = await context.newCDPSession(page);
	await cdp.send('WebAuthn.enable');
	const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', { options: {
		protocol: 'ctap2', transport: 'internal', hasResidentKey: true,
		hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: false
	} });
	await openChat(page);
	await openPasskeySignIn(page);
	const card = connectCard(page);
	await chooseCreateAccount(page);
	await createAccount(page).click();
	await expect(card.getByRole('button', { name: 'Signing in…', exact: true })).toBeVisible();
	await card.getByRole('button', { name: 'Cancel', exact: true }).click();
	await page.getByRole('button', { name: 'Connection settings', exact: true }).click();
	await page.getByTestId('server-url-input').fill('ws://localhost:8080/ws');
	await card.getByRole('button', { name: 'Connect', exact: true }).click();
	await expect(page.getByTestId('connection-status')).toHaveText('Connected');
	await cdp.send('WebAuthn.setAutomaticPresenceSimulation', { authenticatorId, enabled: true });
	await registerPasskey(page);
	expect((await cdp.send('WebAuthn.getCredentials', { authenticatorId })).credentials).toHaveLength(1);
});
