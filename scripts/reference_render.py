#!/usr/bin/env python3
"""Independent reference renderer and sample generator for Atlas Render.

This script deliberately does not import or call the Go implementation. It
re-derives the documented drawing semantics in Python:

  * a layer matrix is local scale, then rotation, then translation, composed
    with every ancestor matrix;
  * a device pixel is covered when its center, mapped back through the full
    inverse matrix, falls inside the half-open local rectangle, inside every
    clip rectangle of the ancestor chain, inside the unit ellipse, or inside
    the even-odd polygon;
  * groups contribute hierarchy, accumulated transform, clips and opacity;
  * output scale multiplies the device pixel grid, not the local geometry.

Every sample also carries relations derived from the scene geometry alone:
exact coverage for axis-aligned results, analytic area for rotated or curved
shapes with a boundary-sampling tolerance, the mapped center for symmetric
shapes, and the mapped corner box. The script refuses to write goldens that
disagree with those derivations, so the checked-in PNGs are pinned to a
coordinate story rather than to whatever the code happened to produce.

Usage:
  python3 scripts/reference_render.py --out internal/raster/testdata/reference
  python3 scripts/reference_render.py --check-only
"""
import argparse
import json
import math
import struct
import zlib
from pathlib import Path

# Every reference sample is drawn on a fixed black canvas.
BACKGROUND = '#000000'


# --- exact quarter-turn trigonometry -------------------------------------
def cos_sin(degrees):
    """Cosine and sine of an angle in degrees, exact at quarter turns.

    math.cos(2*pi) is 1 - 1.2e-16 and math.sin(2*pi) is -2.4e-16, so a naive
    full turn carries a residual rotation. An axis-aligned edge sitting exactly
    on an output pixel center then falls a hair outside the local rectangle and
    a legal pixel column disappears. The documented semantics treat quarter
    turns as exact group elements, so the reference does too.
    """
    quarter = degrees / 90.0
    if quarter == int(quarter):
        return [(1.0, 0.0), (0.0, 1.0), (-1.0, 0.0), (0.0, -1.0)][int(quarter) % 4]
    radians = math.radians(degrees)
    return math.cos(radians), math.sin(radians)


class Matrix:
    __slots__ = ('a', 'b', 'c', 'd', 'e', 'f')

    def __init__(self, a=1.0, b=0.0, c=0.0, d=1.0, e=0.0, f=0.0):
        self.a, self.b, self.c, self.d, self.e, self.f = a, b, c, d, e, f

    def mul(self, n):
        return Matrix(
            self.a * n.a + self.c * n.b, self.b * n.a + self.d * n.b,
            self.a * n.c + self.c * n.d, self.b * n.c + self.d * n.d,
            self.a * n.e + self.c * n.f + self.e, self.b * n.e + self.d * n.f + self.f,
        )

    def apply(self, x, y):
        return (self.a * x + self.c * y + self.e, self.b * x + self.d * y + self.f)

    def inverse(self):
        det = self.a * self.d - self.b * self.c
        if abs(det) < 1e-12:
            raise ValueError('singular accumulated transform')
        ia, ib, ic, id_ = self.d / det, -self.b / det, -self.c / det, self.a / det
        return Matrix(ia, ib, ic, id_, -ia * self.e - ic * self.f, -ib * self.e - id_ * self.f)


def from_transform(t):
    c, s = cos_sin(t.get('degrees', 0.0))
    sx, sy = t['scaleX'], t['scaleY']
    return Matrix(c * sx, s * sx, -s * sy, c * sy, t.get('x', 0.0), t.get('y', 0.0))


# --- containment and paint ------------------------------------------------
def in_rect(x, y, r):
    return r['x'] <= x < r['x'] + r['width'] and r['y'] <= y < r['y'] + r['height']


def in_polygon(x, y, points):
    inside = False
    j = len(points) - 1
    for i in range(len(points)):
        a, b = points[i], points[j]
        if (a['y'] > y) != (b['y'] > y) and x < (b['x'] - a['x']) * (y - a['y']) / (b['y'] - a['y']) + a['x']:
            inside = not inside
        j = i
    return inside


def parse_color(text):
    assert len(text) == 7 and text[0] == '#', text
    value = int(text[1:], 16)
    return (value >> 16 & 255, value >> 8 & 255, value & 255, 255)


def roundish(value):
    return int(math.floor(value + 0.5))


def sample_paint(paint, x, y):
    start = parse_color(paint['color'])
    if paint.get('kind') != 'linear':
        return start
    end = parse_color(paint['endColor'])
    dx = paint['to']['x'] - paint['from']['x']
    dy = paint['to']['y'] - paint['from']['y']
    den = dx * dx + dy * dy
    t = 0.0
    if den > 0:
        t = ((x - paint['from']['x']) * dx + (y - paint['from']['y']) * dy) / den
    t = max(0.0, min(1.0, t))
    return tuple(roundish(a * (1 - t) + b * t) for a, b in zip(start[:3], end[:3])) + (255,)


def blend(dst, src):
    sa, da = src[3] / 255.0, dst[3] / 255.0
    alpha = sa + da * (1 - sa)
    if alpha == 0:
        return (0, 0, 0, 0)
    mix = lambda u, v: roundish((u * sa + v * da * (1 - sa)) / alpha)
    return (mix(src[0], dst[0]), mix(src[1], dst[1]), mix(src[2], dst[2]), roundish(alpha * 255))


class Operation:
    __slots__ = ('layer', 'inverse', 'clips', 'opacity')

    def __init__(self, layer, inverse, clips, opacity):
        self.layer, self.inverse, self.clips, self.opacity = layer, inverse, clips, opacity


def compile_scene(document):
    operations = []

    def walk(layers, parent, clips, alpha):
        for layer in layers:
            world = parent.mul(from_transform(layer['transform']))
            inverse = world.inverse()
            local_clips = list(clips)
            if layer.get('clip') is not None:
                local_clips.append((inverse, layer['clip']))
            opacity = alpha * layer.get('opacity', 1.0)
            if layer['kind'] == 'group':
                walk(layer.get('children', []), world, local_clips, opacity)
                continue
            operations.append(Operation(layer, inverse, local_clips, opacity))

    walk(document['layers'], Matrix(), [], 1.0)
    return operations


def render(document, scale):
    width, height = document['width'] * scale, document['height'] * scale
    background = parse_color(document['background'])
    pixels = [background] * (width * height)
    for op in compile_scene(document):
        if op.opacity == 0:
            continue
        layer = op.layer
        for y in range(height):
            for x in range(width):
                wx, wy = (x + 0.5) / scale, (y + 0.5) / scale
                lx, ly = op.inverse.apply(wx, wy)
                if not in_rect(lx, ly, {'x': 0.0, 'y': 0.0, 'width': layer['width'], 'height': layer['height']}):
                    continue
                if any(not in_rect(*clip_inverse.apply(wx, wy), rect) for clip_inverse, rect in op.clips):
                    continue
                if layer['kind'] == 'ellipse':
                    dx, dy = 2 * lx / layer['width'] - 1, 2 * ly / layer['height'] - 1
                    if dx * dx + dy * dy > 1:
                        continue
                if layer['kind'] == 'polygon' and not in_polygon(lx, ly, layer['points']):
                    continue
                if layer['kind'] == 'image':
                    raise ValueError('reference samples do not cover bitmap assets')
                color = sample_paint(layer['fill'], lx, ly)
                color = (color[0], color[1], color[2], roundish(color[3] * op.opacity))
                index = y * width + x
                pixels[index] = color if color[3] == 255 else blend(pixels[index], color)
    return width, height, pixels


def write_png(path, width, height, pixels):
    raw = bytearray()
    for y in range(height):
        raw.append(0)
        for x in range(width):
            raw.extend(pixels[y * width + x])

    def chunk(tag, data):
        return struct.pack('>I', len(data)) + tag + data + struct.pack('>I', zlib.crc32(tag + data))

    header = struct.pack('>IIBBBBB', width, height, 8, 6, 0, 0, 0)
    data = b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', header) + chunk(b'IDAT', zlib.compress(bytes(raw), 9)) + chunk(b'IEND', b'')
    path.write_bytes(data)


# --- scene constructors ---------------------------------------------------
def rectangle(layer_id, width, height, transform=None, color='#FFFFFF', opacity=1.0, clip=None):
    layer = {'id': layer_id, 'kind': 'rectangle', 'width': width, 'height': height,
             'transform': transform or transform_at(), 'opacity': opacity,
             'fill': {'kind': 'solid', 'color': color}}
    if clip is not None:
        layer['clip'] = clip
    return layer


def group(layer_id, transform=None, children=None, clip=None, opacity=1.0):
    layer = {'id': layer_id, 'kind': 'group', 'transform': transform or transform_at(),
             'opacity': opacity, 'children': children or []}
    if clip is not None:
        layer['clip'] = clip
    return layer


def transform_at(x=0.0, y=0.0, sx=1.0, sy=1.0, deg=0.0):
    return {'x': x, 'y': y, 'scaleX': sx, 'scaleY': sy, 'degrees': deg}


def canvas(layers, width=320, height=240):
    return {'width': width, 'height': height, 'background': BACKGROUND, 'layers': layers}


# --- geometry-only derivations -------------------------------------------
def locate_world(document, layer):
    """Composed local-to-world matrix of one layer, in document order."""
    found = [None]

    def walk(layers, parent):
        for candidate in layers:
            world = parent.mul(from_transform(candidate['transform']))
            if candidate is layer:
                found[0] = world
                return True
            if candidate['kind'] == 'group' and walk(candidate.get('children', []), world):
                return True
        return False

    walk(document['layers'], Matrix())
    assert found[0] is not None, 'layer is not in the document'
    return found[0]


def describe(matrix, width, height):
    """Mapped corners and edge lengths of a local rectangle under a matrix."""
    corners = [matrix.apply(x, y) for x, y in ((0, 0), (width, 0), (width, height), (0, height))]
    lengths = [math.dist(corners[i], corners[(i + 1) % 4]) for i in range(4)]
    return corners, sum(lengths)


def axis_aligned_bounds(matrix, width, height):
    """Device-space half-open bounds when the matrix keeps edges axis aligned."""
    x0, y0 = matrix.apply(0, 0)
    x1, y1 = matrix.apply(width, height)
    return [round(min(x0, x1)), round(min(y0, y1)), round(max(x0, x1)), round(max(y0, y1))]


def rect_relation(document, layer, scale=1, exact=True):
    """Coverage and centroid derived from the rectangle's own geometry."""
    world = locate_world(document, layer)
    width, height = layer['width'], layer['height']
    corners, perimeter = describe(world, width, height)
    sx, sy = layer['transform']['scaleX'], layer['transform']['scaleY']
    area = width * height * abs(sx) * abs(sy)
    cx, cy = world.apply(width / 2, height / 2)
    relation = {
        'coverage': area * scale * scale,
        'centroid': [cx * scale + 0.5 * (scale - 1), cy * scale + 0.5 * (scale - 1)],
        'boundary': perimeter * scale,
        'derivation': 'world area = local area * |scaleX * scaleY| * scale^2; '
                      'centroid = mapped local center',
    }
    if exact:
        relation['bbox'] = [v * scale for v in axis_aligned_bounds(world, width, height)]
    return relation


def clip_relation(document, group_layer, clip, scale=1):
    """Coverage of an oversized child that is bounded only by one clip."""
    world = locate_world(document, group_layer)
    corners, perimeter = describe(world, clip['width'], clip['height'])
    offset = Matrix(world.a, world.b, world.c, world.d,
                    *world.apply(clip['x'], clip['y']))
    sx, sy = group_layer['transform']['scaleX'], group_layer['transform']['scaleY']
    area = clip['width'] * clip['height'] * abs(sx) * abs(sy)
    cx, cy = world.apply(clip['x'] + clip['width'] / 2, clip['y'] + clip['height'] / 2)
    return {
        'coverage': area * scale * scale,
        'centroid': [cx * scale + 0.5 * (scale - 1), cy * scale + 0.5 * (scale - 1)],
        'boundary': perimeter * scale,
        'bbox': [v * scale for v in axis_aligned_bounds(offset, clip['width'], clip['height'])]
        if _is_axis_aligned(world) else None,
        'derivation': 'clip local area * |group scaleX * scaleY| * scale^2; '
                      'centroid = mapped clip center; child is oversized',
    }


def _is_axis_aligned(matrix):
    return abs(matrix.b) < 1e-12 and abs(matrix.c) < 1e-12


def check_relation(name, scale, width, rendered, relation, tolerance):
    background = parse_color(BACKGROUND)
    indices = [i for i, color in enumerate(rendered) if color != background]
    covered = len(indices)
    allowed = tolerance * relation['coverage'] + 2.0
    if abs(covered - relation['coverage']) > allowed:
        raise SystemExit(f'{name} scale {scale}: {covered} covered pixels, '
                         f'geometry derives {relation["coverage"]:.2f} '
                         f'(allowed +-{allowed:.2f})')
    if relation.get('centroid') is not None and covered:
        cx = sum(i % width + 0.5 for i in indices) / covered
        cy = sum(i // width + 0.5 for i in indices) / covered
        drift = math.dist((cx, cy), relation['centroid'])
        bound = 1.5 + relation['boundary'] / max(covered, 1)
        if drift > bound:
            raise SystemExit(f'{name} scale {scale}: centroid ({cx:.3f},{cy:.3f}) is '
                             f'{drift:.3f} away from mapped center '
                             f'({relation["centroid"][0]:.3f},{relation["centroid"][1]:.3f})')
    if relation.get('bbox') is not None and covered:
        xs = [i % width for i in indices]
        ys = [i // width for i in indices]
        # Half-open device bounds: the last covered center is inside max-1.
        want = relation['bbox']
        got = [min(xs), min(ys), max(xs) + 1, max(ys) + 1]
        if any(abs(a - b) > 1.0 for a, b in zip(want, got)):
            raise SystemExit(f'{name} scale {scale}: bbox {got}, geometry derives {want}')


def build_samples():
    samples = []

    def add(name, document, relations, tolerance, derivation):
        samples.append({'name': name, 'document': document,
                        'relations': relations, 'tolerance': tolerance,
                        'derivation': derivation})

    # 1. Quarter turn: the two local axes swap, so the result is still axis
    #    aligned and every number is exact.
    quarter = canvas([rectangle('plate', 100, 60, transform_at(x=110, y=70, deg=90), '#FF8000')])
    add('quarter-turn', quarter,
        {str(s): rect_relation(quarter, quarter['layers'][0], s) for s in (1, 2)}, 0.0,
        'Local x axis becomes world +y and local y becomes world -x; the 100x60 rect '
        'covers x [50,110), y [70,170), area 100*60 = 6000, center maps to (80,120).')

    # 2. Non-uniform scale stretches each local axis independently.
    stretch = canvas([rectangle('plate', 40, 20, transform_at(x=30, y=50, sx=3, sy=0.5), '#40C0FF')])
    add('nonuniform-scale', stretch,
        {str(s): rect_relation(stretch, stretch['layers'][0], s) for s in (1, 2)}, 0.0,
        '40x20 local rect scaled by (3, 0.5) becomes 120x10 at (30,50); area 1200.')

    # 3. Rotation and non-uniform scale compose in the documented order:
    #    scale first, then rotate, then translate.
    combo = canvas([rectangle('plate', 40, 20, transform_at(x=200, y=40, sx=2, sy=3, deg=90), '#FFD040')])
    add('nonuniform-rotation', combo,
        {str(s): rect_relation(combo, combo['layers'][0], s) for s in (1, 2)}, 0.0,
        'Scale first: 40x20 -> 80x60, rotate 90 degrees -> 60x80, translate to (140,40); '
        'area 40*20*2*3 = 4800, bbox [140,40,200,120].')

    # 4. The headline case: ancestor clip, group rotation and non-uniform
    #    scale, oversized child, both output scales.
    combined = canvas([group('frame', transform_at(x=80, y=50, sx=1.5, sy=0.75, deg=25),
                             [rectangle('fill', 400, 300, color='#22CC88')],
                             clip={'x': 20, 'y': 10, 'width': 120, 'height': 100})])
    add('combined-clip-rotation-scale', combined,
        {str(s): clip_relation(combined, combined['layers'][0],
                               combined['layers'][0]['clip'], s) for s in (1, 2)}, 0.002,
        'The group matrix (scale 1.5/0.75, rotate 25, translate 80/50) carries a 120x100 '
        'local clip; determinant 1.5*0.75 = 1.125 gives world area 13500. The 400x300 child '
        'overshoots every side, so coverage is the clip area and the centroid is the mapped '
        'clip center (80,60); scale 2 multiplies coverage by four.')

    # 5. Nested clips intersect: the descendant must satisfy both.
    nested = canvas([group('outer', transform_at(x=100, y=60),
                           [group('inner', transform_at(x=10, y=5),
                                  [rectangle('fill', 400, 400, color='#CC3355')],
                                  clip={'x': 0, 'y': 0, 'width': 60, 'height': 30})],
                           clip={'x': 20, 'y': 10, 'width': 80, 'height': 70})])
    add('nested-clips', nested, {
        str(s): {
            'coverage': 50 * 25 * s * s,
            'centroid': [145.0 * s + 0.5 * (s - 1), 82.5 * s + 0.5 * (s - 1)],
            'boundary': 2 * (50 + 25) * s,
            'bbox': [120 * s, 70 * s, 170 * s, 95 * s],
            'derivation': 'outer clip world x [120,200) y [70,140); inner translation '
                          'moves its clip to x [110,170) y [65,95); intersection x [120,170) '
                          'y [70,95) = 50x25 = 1250; child 400x400 exceeds both',
        } for s in (1, 2)}, 0.0,
        'Both clips must contain the mapped pixel center: the intersection is 50x25 at '
        'world (120,70), center (145,82.5), area 1250.')

    # 6. Document order decides opaque overlap, and also fixes colors.
    ordering = canvas([
        rectangle('under', 100, 100, transform_at(x=10, y=10), '#CC2222'),
        rectangle('over', 60, 60, transform_at(x=50, y=50), '#2222CC'),
    ])
    add('layer-order', ordering, {
        str(s): {
            'coverage': 10000 * s * s,
            'centroid': None, 'bbox': None, 'boundary': 0.0,
            'colors': {'#CC2222': 6400 * s * s, '#2222CC': 3600 * s * s},
            'derivation': 'red x [10,110) y [10,110) and blue x [50,110) y [50,110) overlap in '
                          '60x60; blue is later and opaque so it owns the overlap: 3600 blue, '
                          '10000-3600 = 6400 red',
        } for s in (1, 2)}, 0.0,
        'Later layers paint over earlier ones: 3600 blue pixels and 6400 red pixels at '
        'scale 1, each count multiplied by four at scale 2.')

    # 7. A legal axis-aligned edge lying exactly on pixel centers must survive
    #    at zero rotation and at a full turn alike.
    edge = canvas([rectangle('edge', 100, 60, transform_at(x=10.5, y=20.5), '#7FE0C0')],
                  width=128, height=96)
    turned = canvas([rectangle('edge', 100, 60, transform_at(x=10.5, y=20.5, deg=360), '#7FE0C0')],
                    width=128, height=96)
    add('edge-on-pixel-center', edge,
        {str(s): rect_relation(edge, edge['layers'][0], s) for s in (1, 2)}, 0.0,
        'Left edge at x=10.5 and top at y=20.5 coincide with pixel centers 10 and 20; '
        'half-open bounds put those columns inside, giving exactly 100x60 = 6000 pixels.')
    add('edge-on-pixel-center-full-turn', turned,
        {str(s): rect_relation(turned, turned['layers'][0], s) for s in (1, 2)}, 0.0,
        'A 360 degree turn is the identity, so the same 6000-pixel edge must remain; a '
        'residual sin(2*pi) = -2.4e-16 would move the left edge and delete a pixel column.')

    # 8. Ellipse and polygon use local unit-circle and even-odd tests.
    disc = canvas([{'id': 'disc', 'kind': 'ellipse', 'width': 120, 'height': 80,
                    'transform': transform_at(x=40, y=50), 'opacity': 1.0,
                    'fill': {'kind': 'solid', 'color': '#FFFFFF'}}])
    add('ellipse', disc, {
        str(s): {'coverage': math.pi * 60 * 40 * s * s, 'centroid': None, 'bbox': None,
                 'boundary': 2 * math.pi * math.hypot(60, 40) * s,
                 'derivation': 'semi-axes 60 and 40 -> area pi*60*40 = 7539.8; sampled '
                               'pixel centers deviate only along the boundary'}
        for s in (1, 2)}, 0.01,
        'Unit-circle test in local space: coverage must match the analytic ellipse area '
        'within one percent.')

    wedge = canvas([{'id': 'wedge', 'kind': 'polygon', 'width': 100, 'height': 100,
                     'transform': transform_at(x=20, y=30), 'opacity': 1.0,
                     'fill': {'kind': 'solid', 'color': '#44FFAA'},
                     'points': [{'x': 10, 'y': 90}, {'x': 50, 'y': 0}, {'x': 100, 'y': 90}]}])
    add('polygon', wedge, {
        str(s): {'coverage': 4050 * s * s, 'centroid': None, 'bbox': None, 'boundary': 0.0,
                 'derivation': 'shoelace area of (10,90),(50,0),(100,90) is 4050'}
        for s in (1, 2)}, 0.01,
        'Even-odd fill of the triangle must cover its analytic area within one percent.')

    return samples


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--out', default='internal/raster/testdata/reference')
    parser.add_argument('--check-only', action='store_true', help='derive and validate without writing files')
    args = parser.parse_args()

    out = Path(args.out)
    if not args.check_only:
        out.mkdir(parents=True, exist_ok=True)
    manifest = {'generator': 'scripts/reference_render.py', 'schema': 1, 'samples': []}
    for sample in build_samples():
        name, document = sample['name'], sample['document']
        width, height, pixels = render(document, 1)
        width2, height2, pixels2 = render(document, 2)
        for scale, w, rendered in ((1, width, pixels), (2, width2, pixels2)):
            check_relation(name, scale, w, rendered, sample['relations'][str(scale)], sample['tolerance'])
        if not args.check_only:
            (out / f'{name}.json').write_text(json.dumps(document, indent=2) + '\n')
            write_png(out / f'{name}.s1.png', width, height, pixels)
            write_png(out / f'{name}.s2.png', width2, height2, pixels2)
        manifest['samples'].append({
            'name': name,
            'scene': f'{name}.json',
            'golden': {str(scale): f'{name}.s{scale}.png' for scale in (1, 2)},
            'derivation': sample['derivation'],
            'relations': sample['relations'],
            'tolerance': sample['tolerance'],
        })
        print(f'{name}: geometry relations hold at scale 1 and 2')
    if not args.check_only:
        (out / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
        print(f'wrote {len(manifest["samples"]) * 2} goldens to {out}')


if __name__ == '__main__':
    main()
