"""Render the frames from record.py to a GIF and PNG stills.

Usage: render.py FRAMES_JSON OUTPUT_DIRECTORY

DEMO_FONTS lists directories, separated by ":", that hold the fonts.
"""

import json
import os
import sys
from pathlib import Path

import pyte
from fontTools.ttLib import TTFont
from PIL import Image, ImageDraw, ImageFont

# With record.py's 96 by 28 screen this gives 1992 by 1206 pixels, the size the website declares.
CELL_WIDTH, CELL_HEIGHT, FONT_SIZE = 20, 40, 34
PADDING_X, PADDING_Y = 36, 43
BACKGROUND, FOREGROUND = "#131117", "#f4eef4"
# Changes closer together than this merge into one GIF frame, about 12 per second.
MINIMUM_FRAME_MS = 80
FINAL_HOLD_MS = 4500
# Adwaita Mono supplies the few symbols that Caskaydia Cove lacks.
FONT_FILES = {
    False: ["CaskaydiaCoveNerdFontMono-Regular.ttf", "AdwaitaMono-Regular.ttf"],
    True: ["CaskaydiaCoveNerdFontMono-Bold.ttf", "AdwaitaMono-Regular.ttf"],
}


def find_font(file_name):
    for directory in os.environ["DEMO_FONTS"].split(":"):
        for path in Path(directory).rglob(file_name):
            return path
    raise FileNotFoundError(f"{file_name} is not under DEMO_FONTS")


class Fonts:
    def __init__(self):
        paths = {name: find_font(name) for names in FONT_FILES.values() for name in names}
        self.faces = {name: ImageFont.truetype(str(path), FONT_SIZE) for name, path in paths.items()}
        self.code_points = {name: set(TTFont(path, lazy=True).getBestCmap()) for name, path in paths.items()}

    def face_for(self, character, bold):
        for name in FONT_FILES[bold]:
            if ord(character) in self.code_points[name]:
                return self.faces[name]
        raise LookupError(f"no font has {character!r} (U+{ord(character):04X})")


def cell_color(name, default):
    return "#" + name if len(name) == 6 else default


def draw_block_or_line(draw, character, left, top, color):
    """Draw block and box characters as rectangles, so neighboring cells join without gaps."""
    right, bottom = left + CELL_WIDTH - 1, top + CELL_HEIGHT - 1
    middle_x, middle_y = left + CELL_WIDTH // 2, top + CELL_HEIGHT // 2
    rectangles = {
        "▀": [left, top, right, middle_y - 1],
        "▄": [left, middle_y, right, bottom],
        "█": [left, top, right, bottom],
        "─": [left, middle_y - 1, right, middle_y],
        "│": [middle_x - 1, top, middle_x, bottom],
    }
    if character not in rectangles:
        return False
    draw.rectangle(rectangles[character], fill=color)
    return True


def draw_screen(ansi, columns, rows, fonts):
    screen = pyte.Screen(columns, rows)
    pyte.Stream(screen).feed(ansi.rstrip("\n").replace("\n", "\r\n"))
    image = Image.new("RGB", (columns * CELL_WIDTH + 2 * PADDING_X, rows * CELL_HEIGHT + 2 * PADDING_Y), BACKGROUND)
    draw = ImageDraw.Draw(image)
    for row in range(rows):
        for column in range(columns):
            cell = screen.buffer[row][column]
            foreground, background = cell_color(cell.fg, FOREGROUND), cell_color(cell.bg, BACKGROUND)
            if cell.reverse:
                foreground, background = background, foreground
            left, top = PADDING_X + column * CELL_WIDTH, PADDING_Y + row * CELL_HEIGHT
            if background != BACKGROUND:
                draw.rectangle([left, top, left + CELL_WIDTH - 1, top + CELL_HEIGHT - 1], fill=background)
            character = cell.data
            if not character.strip() or draw_block_or_line(draw, character, left, top, foreground):
                continue
            draw.text((left, top + CELL_HEIGHT // 2), character, fill=foreground, font=fonts.face_for(character, cell.bold), anchor="lm")
    return image


def merge_close_frames(frames):
    merged = []
    for seconds, ansi in frames:
        if merged and (seconds - merged[-1][0]) * 1000 < MINIMUM_FRAME_MS:
            merged[-1] = (merged[-1][0], ansi)
        else:
            merged.append((seconds, ansi))
    return merged


def shared_palette(images):
    """One palette for every frame, so colors do not shift during playback."""
    samples = [images[index * (len(images) - 1) // 5] for index in range(6)]
    sheet = Image.new("RGB", (samples[0].width, samples[0].height * len(samples)))
    for index, sample in enumerate(samples):
        sheet.paste(sample, (0, index * sample.height))
    return sheet.quantize(colors=255, method=Image.Quantize.MEDIANCUT, dither=Image.Dither.NONE)


def write_gif(frames, columns, rows, fonts, path):
    frames = merge_close_frames(frames)
    # GIF delays are in hundredths of a second.
    durations_ms = [round((later[0] - earlier[0]) * 100) * 10 for earlier, later in zip(frames, frames[1:])]
    durations_ms.append(FINAL_HOLD_MS)
    images = [draw_screen(ansi, columns, rows, fonts) for _, ansi in frames]
    palette = shared_palette(images)
    quantized = [image.quantize(palette=palette, dither=Image.Dither.NONE) for image in images]
    quantized[0].save(path, save_all=True, append_images=quantized[1:], duration=durations_ms, loop=0, disposal=1)
    print(f"{path}: {len(quantized)} frames, {sum(durations_ms) / 1000:.1f} seconds, {images[0].width}x{images[0].height}")


def main():
    recording = json.loads(Path(sys.argv[1]).read_text())
    output = Path(sys.argv[2])
    columns, rows, fonts = recording["columns"], recording["rows"], Fonts()
    write_gif(recording["frames"], columns, rows, fonts, output / "workbench.gif")
    for name, ansi in recording["stills"].items():
        draw_screen(ansi, columns, rows, fonts).save(output / f"{name}.png")
        print(output / f"{name}.png")


main()
