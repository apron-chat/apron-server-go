import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { defineConfig, devices } from '@playwright/test';

/**
 * Web push and status interop (push.spec.ts), apart from the main suite because aprond needs
 * push flags for it: a fixed VAPID key, whose public half the test checks tokens against, and
 * --push.allow-insecure, so the test's capture endpoint on http://127.0.0.1 is accepted.
 *
 *   npx playwright test --config=push.config.ts
 *
 * Notifications need full Chromium in its new headless mode: the headless shell reports
 * Notification.permission as denied whatever is granted.
 */
export const VAPID_PRIVATE_KEY = 'bXTr5nHrl6nDHBL4V4g_XLy6WRhXXezSvZ3Dm3_YilM';
export const VAPID_SUBJECT = 'mailto:interop@example.com';

const configDirectory = path.dirname(fileURLToPath(import.meta.url));
const repositoryRoot = path.resolve(configDirectory, '../..');
const chromiumExecutablePath = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH;

export default defineConfig({
	testDir: configDirectory,
	testMatch: /push\.spec\.ts$/,
	fullyParallel: false,
	workers: 1,
	retries: process.env.CI ? 1 : 0,
	// A connection turns idle only after 30 seconds unattended (§4.11), and the tests wait for it.
	timeout: 180_000,
	expect: { timeout: 10_000 },
	reporter: process.env.CI ? [['line'], ['html', { open: 'never' }]] : 'list',
	use: {
		...devices['Desktop Chrome'],
		// Passkeys need the default RP ID, localhost.
		baseURL: 'http://localhost:5173',
		trace: 'retain-on-failure',
		screenshot: 'only-on-failure',
		...(chromiumExecutablePath ? { launchOptions: { executablePath: chromiumExecutablePath } } : { channel: 'chromium' })
	},
	webServer: [
		{
			command: `go run ./cmd/aprond --store memory --addr 127.0.0.1:8080 --push.allow-insecure --push.vapid-private-key ${VAPID_PRIVATE_KEY} --push.vapid-subject ${VAPID_SUBJECT}`,
			cwd: repositoryRoot,
			url: 'http://127.0.0.1:8080/healthz',
			timeout: 120_000,
			reuseExistingServer: false
		},
		{
			command: 'npm run dev -- --host 127.0.0.1 --port 5173 --strictPort',
			cwd: process.env.APRON_WEB_DIR ?? path.join(repositoryRoot, '.apron-web'),
			url: 'http://127.0.0.1:5173',
			timeout: 120_000,
			reuseExistingServer: false
		}
	]
});
