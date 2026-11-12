// Renders the Scout app icon as a 1024x1024 PNG.
// Usage: swift scripts/make_icon.swift out.png
import AppKit
import CoreGraphics

let size: CGFloat = 1024
let out = CommandLine.arguments.count > 1 ? CommandLine.arguments[1] : "icon.png"
let space = CGColorSpace(name: CGColorSpace.sRGB)!
let ctx = CGContext(data: nil, width: Int(size), height: Int(size), bitsPerComponent: 8, bytesPerRow: 0,
                    space: space, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!

func rgb(_ r: CGFloat, _ g: CGFloat, _ b: CGFloat, _ a: CGFloat = 1) -> CGColor {
    CGColor(srgbRed: r / 255, green: g / 255, blue: b / 255, alpha: a)
}
func rounded(_ r: CGRect, _ radius: CGFloat) -> CGPath {
    CGPath(roundedRect: r, cornerWidth: radius, cornerHeight: radius, transform: nil)
}

// macOS icon grid: 824pt body centred in 1024 with a soft drop shadow.
let body = CGRect(x: 100, y: 100, width: 824, height: 824)
let squircle = rounded(body, 185)
ctx.saveGState()
ctx.setShadow(offset: CGSize(width: 0, height: -14), blur: 28, color: rgb(0, 0, 0, 0.45))
ctx.addPath(squircle)
ctx.setFillColor(rgb(12, 16, 24))
ctx.fillPath()
ctx.restoreGState()

// Background: deep navy to teal, clipped to the squircle.
ctx.saveGState()
ctx.addPath(squircle)
ctx.clip()
let bg = CGGradient(colorsSpace: space, colors: [rgb(10, 18, 32), rgb(14, 52, 72)] as CFArray, locations: [0, 1])!
ctx.drawLinearGradient(bg, start: CGPoint(x: 512, y: 924), end: CGPoint(x: 512, y: 100), options: [])
// Floor glow under the rover.
let glow = CGGradient(colorsSpace: space, colors: [rgb(79, 209, 255, 0.35), rgb(79, 209, 255, 0)] as CFArray, locations: [0, 1])!
ctx.drawRadialGradient(glow, startCenter: CGPoint(x: 512, y: 300), startRadius: 0,
                       endCenter: CGPoint(x: 512, y: 300), endRadius: 360, options: [])

// HUD arc and tick marks behind the rover.
ctx.setStrokeColor(rgb(79, 209, 255, 0.28))
ctx.setLineWidth(10)
ctx.setLineCap(.round)
ctx.addArc(center: CGPoint(x: 512, y: 470), radius: 310, startAngle: .pi * 0.18, endAngle: .pi * 0.82, clockwise: false)
ctx.strokePath()
for a in stride(from: 0.26, through: 0.74, by: 0.08) {
    let angle = CGFloat.pi * CGFloat(a)
    let p1 = CGPoint(x: 512 + cos(angle) * 330, y: 470 + sin(angle) * 330)
    let p2 = CGPoint(x: 512 + cos(angle) * 352, y: 470 + sin(angle) * 352)
    ctx.move(to: p1); ctx.addLine(to: p2)
}
ctx.setLineWidth(8)
ctx.strokePath()
ctx.restoreGState()

// Rover: chassis, wheel tracks, camera head.
func fill(_ path: CGPath, _ colors: [CGColor], top: CGFloat, bottom: CGFloat) {
    ctx.saveGState()
    ctx.addPath(path); ctx.clip()
    let g = CGGradient(colorsSpace: space, colors: colors as CFArray, locations: [0, 1])!
    ctx.drawLinearGradient(g, start: CGPoint(x: 0, y: top), end: CGPoint(x: 0, y: bottom), options: [])
    ctx.restoreGState()
}
// Tracks
for x in [262.0, 610.0] {
    let track = rounded(CGRect(x: x, y: 250, width: 152, height: 112), 56)
    fill(track, [rgb(58, 66, 80), rgb(26, 30, 38)], top: 362, bottom: 250)
    for i in 0..<3 {
        ctx.addEllipse(in: CGRect(x: x + 22 + CGFloat(i) * 40, y: 284, width: 28, height: 28))
    }
    ctx.setFillColor(rgb(12, 14, 18, 0.9)); ctx.fillPath()
}
// Chassis
let chassis = rounded(CGRect(x: 232, y: 318, width: 560, height: 210), 88)
ctx.saveGState()
ctx.setShadow(offset: CGSize(width: 0, height: -10), blur: 24, color: rgb(0, 0, 0, 0.5))
ctx.addPath(chassis); ctx.setFillColor(rgb(230, 236, 244)); ctx.fillPath()
ctx.restoreGState()
fill(chassis, [rgb(246, 249, 252), rgb(196, 206, 218)], top: 528, bottom: 318)
// Front visor band
let visor = rounded(CGRect(x: 290, y: 372, width: 444, height: 104), 52)
fill(visor, [rgb(20, 26, 36), rgb(8, 10, 14)], top: 476, bottom: 372)
// Camera eye with glow
let eye = CGPoint(x: 512, y: 424)
let eyeGlow = CGGradient(colorsSpace: space, colors: [rgb(79, 209, 255, 0.9), rgb(79, 209, 255, 0)] as CFArray, locations: [0, 1])!
ctx.drawRadialGradient(eyeGlow, startCenter: eye, startRadius: 0, endCenter: eye, endRadius: 120, options: [])
ctx.addEllipse(in: CGRect(x: eye.x - 46, y: eye.y - 46, width: 92, height: 92))
ctx.setFillColor(rgb(150, 232, 255)); ctx.fillPath()
ctx.addEllipse(in: CGRect(x: eye.x - 22, y: eye.y - 22, width: 44, height: 44))
ctx.setFillColor(rgb(10, 40, 60)); ctx.fillPath()
ctx.addEllipse(in: CGRect(x: eye.x - 2, y: eye.y + 6, width: 16, height: 16))
ctx.setFillColor(rgb(255, 255, 255, 0.9)); ctx.fillPath()
// Status lights
for (i, c) in [rgb(61, 220, 132), rgb(79, 209, 255)].enumerated() {
    ctx.addEllipse(in: CGRect(x: 338 + CGFloat(i) * 300 + (i == 1 ? 18 : 0), y: 414, width: 20, height: 20))
    ctx.setFillColor(c); ctx.fillPath()
}
// Antenna
ctx.setStrokeColor(rgb(196, 206, 218)); ctx.setLineWidth(12); ctx.setLineCap(.round)
ctx.move(to: CGPoint(x: 700, y: 520)); ctx.addLine(to: CGPoint(x: 744, y: 628)); ctx.strokePath()
ctx.addEllipse(in: CGRect(x: 728, y: 618, width: 32, height: 32))
ctx.setFillColor(rgb(255, 176, 32)); ctx.fillPath()

let image = ctx.makeImage()!
let rep = NSBitmapImageRep(cgImage: image)
try! rep.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: out))
print(out)
