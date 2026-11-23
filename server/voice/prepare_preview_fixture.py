"""Prepare the bundled test icon for the experimental RGB565 screen path."""
from pathlib import Path
from PIL import Image

source = Path(__file__).resolve().parent.parent / 'stackchan-assets' / 'icon.png'
target = Path(__file__).resolve().parent / 'internal/service/ai/assets/preview-test.rgb565'
image = Image.open(source).convert('RGBA').resize((240, 240), Image.Resampling.LANCZOS)
background = Image.new('RGBA', image.size, (0, 0, 0, 255))
background.alpha_composite(image)
output = bytearray()
for red, green, blue in background.convert('RGB').getdata():
    rgb565 = ((red >> 3) << 11) | ((green >> 2) << 5) | (blue >> 3)
    output.extend(rgb565.to_bytes(2, 'little'))
assert len(output) == 240 * 240 * 2
target.write_bytes(output)
print(f'Prepared {len(output)} bytes of RGB565 test image')
