// BrandMark is the Open Factory logo: two equal squares that touch at one
// corner (docs/assets/brand). The ink square follows the text color, so the
// mark works in light and dark themes; the accent square is always orange.
export function BrandMark({ size = 20 }: { size?: number }) {
  return (
    <svg
      className="brand-mark"
      width={size}
      height={size}
      viewBox="0 0 48 48"
      aria-hidden="true"
      focusable="false"
    >
      <rect x="0" y="24" width="24" height="24" fill="currentColor" />
      <rect x="24" y="0" width="24" height="24" fill="#FF5A1F" />
    </svg>
  );
}

// BrandLogo is the full logo: the mark and the "openfactory" wordmark in
// Space Grotesk SemiBold, lowercase, with -0.03em letter spacing.
export function BrandLogo({ size = 20 }: { size?: number }) {
  return (
    <span className="brand-logo" style={{ fontSize: Math.round(size * 0.9) }}>
      <BrandMark size={size} />
      <span className="brand-word">openfactory</span>
    </span>
  );
}
