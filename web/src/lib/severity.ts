// Maps the seven OTel severity strings ingest/internal/normalize writes
// (see /storage/README.md) onto the four visual accent tiers plus one
// "quiet" state design-system.md documents. Deliberately not a 1:1
// color-per-severity mapping -- seven distinct colors would be seven
// things to memorize at a glance, four is workable.

export type SeverityTier = 'quiet' | 'info' | 'warn' | 'error' | 'critical';

const TIER_BY_SEVERITY: Record<string, SeverityTier> = {
	TRACE: 'quiet',
	DEBUG: 'quiet',
	UNSPECIFIED: 'quiet',
	INFO: 'info',
	WARN: 'warn',
	ERROR: 'error',
	FATAL: 'critical'
};

export function severityTier(severity: string | undefined | null): SeverityTier {
	if (!severity) return 'quiet';
	return TIER_BY_SEVERITY[severity.toUpperCase()] ?? 'quiet';
}
