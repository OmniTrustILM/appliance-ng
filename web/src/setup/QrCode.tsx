import qrcode from 'qrcode-generator';

const quietZone = 4;

/** Draws text as a QR code in the browser; nothing is sent anywhere. */
export function QrCode({ value, label }: { value: string; label: string }) {
  const code = qrcode(0, 'M');
  code.addData(value);
  code.make();
  const count = code.getModuleCount();
  const size = count + 2 * quietZone;
  let path = '';
  for (let row = 0; row < count; row++) {
    for (let column = 0; column < count; column++) {
      if (code.isDark(row, column)) path += `M${column + quietZone} ${row + quietZone}h1v1h-1z`;
    }
  }
  return (
    <svg role="img" aria-label={label} viewBox={`0 0 ${size} ${size}`} className="qr" shapeRendering="crispEdges">
      <rect width={size} height={size} fill="#fff" />
      <path d={path} fill="#000" />
    </svg>
  );
}
