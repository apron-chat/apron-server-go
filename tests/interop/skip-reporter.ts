import fs from 'node:fs';
import type { Reporter, TestCase, TestResult } from '@playwright/test/reporter';

/**
 * Lists skipped tests with their reasons at the end of a run, which the line and list reporters
 * leave out, so a test skipped for a missing web client feature (push.spec.ts before
 * apron-web#48) is visible. On GitHub Actions each also becomes a notice and a line in the job
 * summary.
 */
export default class SkipReporter implements Reporter {
	private skipped: string[] = [];

	onTestEnd(test: TestCase, result: TestResult) {
		if (result.status !== 'skipped') return;
		const reason = test.annotations.find((annotation) => annotation.type === 'skip')?.description ?? 'no reason given';
		this.skipped.push(`${test.titlePath().filter(Boolean).join(' › ')}: ${reason}`);
	}

	onEnd() {
		if (!this.skipped.length) return;
		console.log(`\n${this.skipped.length} skipped:`);
		for (const line of this.skipped) console.log(`  - ${line}`);
		if (!process.env.GITHUB_ACTIONS) return;
		for (const line of this.skipped) console.log(`::notice title=Interop test skipped::${line.replace(/%/g, '%25').replace(/\r/g, '%0D').replace(/\n/g, '%0A')}`);
		if (process.env.GITHUB_STEP_SUMMARY) {
			fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY, `### Skipped interop tests\n\n${this.skipped.map((line) => `- ${line}`).join('\n')}\n`);
		}
	}

	printsToStdio() {
		return false;
	}
}
