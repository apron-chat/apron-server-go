/**
 * `seen` (§4.5.2) between the web client and this server: when an account's shown status last
 * became offline, as another browser shows it. The push project of playwright.config.ts runs it
 * with the status test, on localhost for passkeys:
 *   npx playwright test --project=push seen
 *
 * The subject is an account, signed up with a passkey on Chromium's virtual authenticator: a
 * guest is retired when its connection ends (a reconnect is a new guest), so it is never shown
 * offline after it leaves. The watcher is the web client as a guest in another browser context.
 */
import { expect, test, type Browser, type BrowserContext, type Locator, type Page } from '@playwright/test';
import { openChat, signUpWithPasskey, userIdOf } from './test-helpers';

const ORIGIN = 'http://localhost:5173';
/** How long the server may take to see a closed tab and tell others. */
const SHOWN = { timeout: 15_000 };

interface Subject { context: BrowserContext; page: Page; userId: string }

/** A new browser whose page signs up for an account with a passkey, in General. */
async function signUpSubject(browser: Browser): Promise<Subject> {
	const context = await browser.newContext({ baseURL: ORIGIN });
	const page = await context.newPage();
	await openChat(page);
	await signUpWithPasskey(page, context);
	return { context, page, userId: await userIdOf(page) };
}

/** The member list of the open room, shown when it is behind a toggle. */
async function memberList(page: Page): Promise<Locator> {
	const list = page.getByTestId('room-member-list');
	const toggle = page.getByRole('button', { name: 'Show member list' });
	if (!(await list.isVisible()) && await toggle.count()) await toggle.first().click();
	await expect(list).toBeVisible();
	return list;
}

const memberRow = (list: Locator, userId: string) => list.locator(`li[data-user="${userId}"]`);

/** Opens a member's profile card from their member list row, closing one already open. */
async function profileCard(page: Page, list: Locator, userId: string): Promise<Locator> {
	const card = page.getByTestId('profile-card');
	if (await card.count()) {
		await page.keyboard.press('Escape');
		await expect(card).toHaveCount(0);
	}
	await memberRow(list, userId).locator(`button[data-user-id="${userId}"]`).click();
	await expect(card).toBeVisible();
	return card;
}

/** The card's "Last seen" as its `<time datetime>` says it, in Unix milliseconds. */
async function seenAt(card: Locator): Promise<number> {
	const seen = card.getByTestId('profile-seen');
	await expect(seen).toHaveText('Last seen just now');
	const datetime = await seen.getAttribute('datetime');
	expect(datetime, 'profile-seen has a datetime').toBeTruthy();
	return Date.parse(datetime!);
}

/**
 * Chooses a status in Preferences' Status menu, which the web client sets with `me` (§4.5), and waits until
 * the server has kept it: the row's hint then says how others see you.
 */
async function chooseStatus(page: Page, choice: RegExp, hint: string): Promise<void> {
	await page.getByRole('button', { name: /^Open preferences/ }).click();
	const prefs = page.getByRole('dialog', { name: 'Preferences' });
	await prefs.getByRole('button', { name: /^Status: / }).click();
	await page.getByRole('menuitemradio', { name: choice }).click();
	await expect(prefs.getByTestId('status-hint')).toHaveText(hint);
	await page.keyboard.press('Escape');
	await expect(prefs).toHaveCount(0);
}

test('an account that leaves, comes back, and goes invisible shows Last seen only while offline', async ({ page, browser }) => {
	const subject = await signUpSubject(browser);
	const { userId } = subject;
	await openChat(page);
	const list = await memberList(page);
	const row = memberRow(list, userId);
	await expect(row).toHaveAttribute('data-status', 'online', SHOWN);
	let card = await profileCard(page, list, userId);
	await expect(card.getByTestId('profile-status')).toHaveText('Online');
	await expect(card.getByTestId('profile-seen')).toHaveCount(0);

	// Its last tab closes: shown offline, last seen when it closed.
	const closing = Date.now();
	await subject.page.close();
	await expect(row).toHaveAttribute('data-status', 'offline', SHOWN);
	const shownOffline = Date.now();
	card = await profileCard(page, list, userId);
	await expect(card.getByTestId('profile-status')).toHaveText('Offline');
	const left = await seenAt(card);
	expect(left, 'seen is when the tab closed').toBeGreaterThanOrEqual(closing - 1_000);
	expect(left, 'seen is when the tab closed').toBeLessThanOrEqual(shownOffline + 1_000);
	await expect(row.locator(`button[data-user-id="${userId}"]`)).toHaveAttribute('title', /· Offline · last seen just now$/);

	// It comes back, resuming its session in a new tab: online, and the seen it keeps says nothing.
	const back = await subject.context.newPage();
	await openChat(back);
	expect(await userIdOf(back)).toBe(userId);
	await expect(row).toHaveAttribute('data-status', 'online', SHOWN);
	card = await profileCard(page, list, userId);
	await expect(card.getByTestId('profile-status')).toHaveText('Online');
	await expect(card.getByTestId('profile-seen')).toHaveCount(0);

	// It chooses invisible: shown offline while connected, last seen when it chose.
	const choosing = Date.now();
	await chooseStatus(back, /^Invisible/, 'Others see you as offline.');
	await expect(row).toHaveAttribute('data-status', 'offline', SHOWN);
	const shownInvisible = Date.now();
	card = await profileCard(page, list, userId);
	await expect(card.getByTestId('profile-status')).toHaveText('Offline');
	const hidden = await seenAt(card);
	expect(hidden, 'seen is when it chose invisible').toBeGreaterThanOrEqual(Math.max(left, choosing - 1_000));
	expect(hidden, 'seen is when it chose invisible').toBeLessThanOrEqual(shownInvisible + 1_000);

	// While invisible, its tab closes and another opens: others are told nothing, so the row stays
	// offline and Last seen stays when it chose invisible, telling nothing of its connections. The
	// server sends nothing to wait for, so each step gets a moment to arrive if it were sent.
	await back.close();
	await page.waitForTimeout(1_000);
	await expect(row).toHaveAttribute('data-status', 'offline');
	card = await profileCard(page, list, userId);
	expect(await seenAt(card), 'seen stays when an invisible account leaves').toBe(hidden);
	const again = await subject.context.newPage();
	await openChat(again);
	expect(await userIdOf(again)).toBe(userId);
	await page.waitForTimeout(1_000);
	await expect(row).toHaveAttribute('data-status', 'offline');
	card = await profileCard(page, list, userId);
	await expect(card.getByTestId('profile-status')).toHaveText('Offline');
	expect(await seenAt(card), 'seen stays when an invisible account comes back').toBe(hidden);

	// Online again: no Last seen.
	await chooseStatus(again, /^Online/, 'Others see you online, idle or offline, as you come and go.');
	await expect(row).toHaveAttribute('data-status', 'online', SHOWN);
	card = await profileCard(page, list, userId);
	await expect(card.getByTestId('profile-seen')).toHaveCount(0);
	await subject.context.close();
	await expect(row).toHaveAttribute('data-status', 'offline', SHOWN);
});

test('a browser that opens after an account left learns when it was last seen from the listing', async ({ page, browser }) => {
	const subject = await signUpSubject(browser);
	const { userId } = subject;
	// Another guest watches it go, so the test knows when the server has seen the tab close.
	const watcher = await browser.newContext({ baseURL: ORIGIN });
	const watching = await watcher.newPage();
	await openChat(watching);
	const watchedRow = memberRow(await memberList(watching), userId);
	await expect(watchedRow).toHaveAttribute('data-status', 'online', SHOWN);
	const closing = Date.now();
	await subject.context.close();
	await expect(watchedRow).toHaveAttribute('data-status', 'offline', SHOWN);
	const shownOffline = Date.now();
	await watcher.close();

	// The new browser's room_list carries the member offline with its seen.
	await openChat(page);
	const list = await memberList(page);
	await expect(memberRow(list, userId)).toHaveAttribute('data-status', 'offline');
	const card = await profileCard(page, list, userId);
	await expect(card.getByTestId('profile-status')).toHaveText('Offline');
	const left = await seenAt(card);
	expect(left).toBeGreaterThanOrEqual(closing - 1_000);
	expect(left).toBeLessThanOrEqual(shownOffline + 1_000);
});
