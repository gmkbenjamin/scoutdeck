// scout-voice: on-device voice assistant for Scout.
//
// Listens to the robot's microphone (or this Mac's with --source mac), wakes
// on "Scout", lets Apple's on-device language model act through the Scout
// agent API, and answers through the robot's speaker. Speech recognition, the
// language model and speech synthesis all run on this Mac.
//
// Environment: SCOUT_API_URL (default http://127.0.0.1:8765), SCOUT_API_TOKEN.
// Prints one JSON event per line on stdout for the Scout app.

import AVFoundation
import Foundation
import FoundationModels
import Speech

// MARK: - Events

enum Event {
    static func emit(_ kind: String, _ text: String = "") {
        let obj: [String: String] = ["event": kind, "text": text]
        if let data = try? JSONSerialization.data(withJSONObject: obj), let line = String(data: data, encoding: .utf8) {
            FileHandle.standardOutput.write((line + "\n").data(using: .utf8)!)
        }
    }
}

// MARK: - Scout API

struct APIError: Error, CustomStringConvertible {
    let status: Int
    let message: String
    var description: String { message }
}

final class ScoutAPI: @unchecked Sendable {
    let base: URL
    let token: String
    let session: URLSession

    init() throws {
        let env = ProcessInfo.processInfo.environment
        base = URL(string: env["SCOUT_API_URL"] ?? "http://127.0.0.1:8765")!
        var token = env["SCOUT_API_TOKEN"] ?? ""
        if token.isEmpty {
            let file = FileManager.default.homeDirectoryForCurrentUser
                .appendingPathComponent("Library/Application Support/Scout/api-token")
            token = (try? String(contentsOf: file, encoding: .utf8))?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        }
        guard !token.isEmpty else { throw APIError(status: 0, message: "no API token") }
        self.token = token
        let config = URLSessionConfiguration.ephemeral
        config.timeoutIntervalForRequest = 15
        session = URLSession(configuration: config)
    }

    func request(_ method: String, _ path: String, json: [String: Any]? = nil, body: Data? = nil, contentType: String? = nil) -> URLRequest {
        var req = URLRequest(url: base.appendingPathComponent(path))
        req.httpMethod = method
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        if let json {
            req.httpBody = try? JSONSerialization.data(withJSONObject: json)
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        } else if let body {
            req.httpBody = body
            req.setValue(contentType, forHTTPHeaderField: "Content-Type")
        }
        return req
    }

    @discardableResult
    func call(_ method: String, _ path: String, json: [String: Any]? = nil, body: Data? = nil, contentType: String? = nil) async throws -> [String: Any] {
        let (data, response) = try await session.data(for: request(method, path, json: json, body: body, contentType: contentType))
        let status = (response as? HTTPURLResponse)?.statusCode ?? 0
        let obj = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] ?? [:]
        guard (200..<300).contains(status) else {
            throw APIError(status: status, message: obj["error"] as? String ?? "HTTP \(status)")
        }
        return obj
    }
}

// MARK: - Robot actions (shared by the tools)

actor Robot {
    let api: ScoutAPI
    let dryRun: Bool
    private var motion: Task<Void, Error>?

    init(api: ScoutAPI, dryRun: Bool) { self.api = api; self.dryRun = dryRun }

    /// Sends short drive leases until `seconds` elapse or `stop()` is called.
    func drive(x: Double, y: Double, z: Double, seconds: Double) async throws {
        if dryRun {
            Event.emit("dry-run", String(format: "drive x=%.2f y=%.2f z=%.2f for %.1fs", x, y, z, seconds))
            return
        }
        motion?.cancel()
        let api = self.api
        let task = Task {
            let end = Date().addingTimeInterval(seconds)
            defer { Task { try? await api.call("POST", "v1/drive", json: ["linear_x": 0, "linear_y": 0, "angular_z": 0, "ttl_ms": 100]) } }
            while Date() < end {
                try Task.checkCancellation()
                try await api.call("POST", "v1/drive", json: ["linear_x": x, "linear_y": y, "angular_z": z, "ttl_ms": 300])
                try await Task.sleep(for: .milliseconds(100))
            }
        }
        motion = task
        try await task.value
    }

    private var goalActive = false
    var moving: Bool { goalActive || (motion.map { !$0.isCancelled } ?? false) }

    /// Runs a closed-loop move or turn in the Mac app (/v1/move, /v1/turn),
    /// which stops on its own at the target, at an obstacle, or on /v1/stop.
    func goal(_ path: String, _ json: [String: Any]) async throws -> [String: Any] {
        if dryRun {
            Event.emit("dry-run", "POST \(path) \(json)")
            return ["completed": true, "achieved": json["distance_m"] ?? json["strafe_m"] ?? ((json["degrees"] as? Double ?? 0) * .pi / 180)]
        }
        goalActive = true
        defer { goalActive = false }
        return try await api.call("POST", path, json: json)
    }

    /// API calls that change the robot (docking, lights); skipped in dry runs.
    @discardableResult
    func act(_ path: String, _ json: [String: Any]) async throws -> [String: Any] {
        if dryRun {
            Event.emit("dry-run", "POST \(path) \(json)")
            return [:]
        }
        return try await api.call("POST", path, json: json)
    }

    func stop() async {
        motion?.cancel()
        motion = nil
        if dryRun { return }
        // Cancels a running move, turn or patrol in the Mac app as well.
        try? await api.call("POST", "v1/stop")
    }

    func say(_ text: String) async {
        Event.emit("speaking", text)
        do {
            let wav = try await Speech16k.synthesize(text)
            try await api.call("POST", "v1/speaker.wav", body: wav, contentType: "audio/wav")
        } catch {
            Event.emit("error", "speaker: \(error)")
        }
        Event.emit("listening")
    }
}

func friendly(_ error: Error) -> String {
    if let e = error as? APIError {
        if e.status == 409 { return "I can't move right now: \(e.message)." }
        if e.status == 503 { return "The robot isn't connected." }
        return e.message
    }
    if error is CancellationError { return "Stopped." }
    return "\(error)"
}

// MARK: - Tools for the on-device model

struct MoveTool: Tool {
    let robot: Robot
    let name = "move"
    let description = "Drive the robot in a straight line without turning. Use for forward, backward, or sliding sideways."

    @Generable
    struct Arguments {
        @Guide(description: "Direction of travel", .anyOf(["forward", "backward", "left", "right"]))
        var direction: String
        @Guide(description: "Distance in meters", .range(0.05...2.0))
        var meters: Double
    }

    func call(arguments: Arguments) async throws -> String {
        let m = arguments.meters
        var json: [String: Any] = [:]
        switch arguments.direction {
        case "forward": json["distance_m"] = m
        case "backward": json["distance_m"] = -m
        case "left": json["strafe_m"] = m
        default: json["strafe_m"] = -m
        }
        Event.emit("action", "move \(arguments.direction) \(String(format: "%.2f", m)) m")
        do {
            let r = try await robot.goal("v1/move", json)
            let done = abs(r["achieved"] as? Double ?? 0)
            if r["completed"] as? Bool ?? false {
                return "Moved \(arguments.direction) \(spokenDistance(done))."
            }
            return stoppedReason(r, moved: "after \(spokenDistance(done))")
        } catch {
            return friendly(error)
        }
    }
}

func spokenDistance(_ m: Double) -> String {
    m < 1 ? "\(Int((m * 100).rounded())) centimeters" : String(format: "%.1f meters", m)
}

func stoppedReason(_ r: [String: Any], moved: String) -> String {
    switch r["stopped_by"] as? String ?? "" {
    case "obstacle": return "I stopped \(moved) because something is in the way."
    case "manual": return "I stopped \(moved) because someone took the controls."
    case "stalled": return "I stopped \(moved); I seem to be stuck."
    case "timeout": return "I gave up \(moved); it was taking too long."
    case "cancelled": return "Stopped \(moved)."
    default: return "I couldn't finish: \(r["error"] as? String ?? "unknown problem")."
    }
}

struct TurnTool: Tool {
    let robot: Robot
    let name = "turn"
    let description = "Rotate the robot in place to the left or right by a number of degrees."

    @Generable
    struct Arguments {
        @Guide(description: "Which way to rotate", .anyOf(["left", "right"]))
        var direction: String
        @Guide(description: "Angle in degrees, 90 is a quarter turn", .range(5...360))
        var degrees: Int
    }

    func call(arguments: Arguments) async throws -> String {
        let degrees = Double(arguments.degrees) * (arguments.direction == "left" ? 1 : -1)
        Event.emit("action", "turn \(arguments.direction) \(arguments.degrees)°")
        do {
            let r = try await robot.goal("v1/turn", ["degrees": degrees])
            let done = Int((abs(r["achieved"] as? Double ?? 0) * 180 / .pi).rounded())
            if r["completed"] as? Bool ?? false {
                return "Turned \(arguments.direction) \(done) degrees."
            }
            return stoppedReason(r, moved: "after \(done) degrees")
        } catch {
            return friendly(error)
        }
    }
}

struct StopTool: Tool {
    let robot: Robot
    let name = "stop"
    let description = "Stop all movement immediately."

    @Generable
    struct Arguments {}

    func call(arguments: Arguments) async throws -> String {
        Event.emit("action", "stop")
        await robot.stop()
        return "Stopped."
    }
}

struct DockTool: Tool {
    let robot: Robot
    let name = "dock"
    let description = "Drive back to the charging dock by itself and start charging. Takes a few minutes."

    @Generable
    struct Arguments {}

    func call(arguments: Arguments) async throws -> String {
        Event.emit("action", "dock")
        do {
            try await robot.act("v1/dock", ["timeout_seconds": 300])
            return "Docking started; it can take a few minutes."
        } catch {
            return friendly(error)
        }
    }
}

struct LightTool: Tool {
    let robot: Robot
    let name = "light"
    let description = "Change the robot's night-vision light."

    @Generable
    struct Arguments {
        @Guide(description: "brighter, dimmer, or automatic", .anyOf(["brighter", "dimmer", "automatic"]))
        var setting: String
    }

    func call(arguments: Arguments) async throws -> String {
        let command = ["brighter": 1, "dimmer": 0, "automatic": 4][arguments.setting] ?? 4
        Event.emit("action", "light \(arguments.setting)")
        do {
            try await robot.act("v1/light", ["command": command])
            return "Light set to \(arguments.setting)."
        } catch {
            return friendly(error)
        }
    }
}

struct StatusTool: Tool {
    let robot: Robot
    let name = "status"
    let description = "Read the robot's battery level, charging and docking state, distance to the nearest obstacle ahead, and guard mode."

    @Generable
    struct Arguments {}

    func call(arguments: Arguments) async throws -> String {
        Event.emit("action", "status")
        do {
            let s = try await robot.api.call("GET", "v1/status")
            let battery = s["battery"] as? [String: Any] ?? [:]
            let pct = battery["percent"] as? Int ?? -1
            let charging = ["charging", "on battery", "full"][safe: battery["charge_state"] as? Int ?? 9] ?? "unknown"
            let dock = (s["docking"] as? [String: Any])?["state"] as? String ?? "idle"
            let sensors = s["sensors"] as? [String: Any] ?? [:]
            var ahead = "Nothing within 2 meters ahead."
            if let d = sensors["obstacle_m"] as? Double { ahead = "Something is \(spokenDistance(d)) ahead." }
            let guardArmed = (s["guard"] as? [String: Any])?["armed"] as? Bool ?? false
            return "Battery \(pct >= 0 ? "\(pct)%" : "unknown"), \(charging). Docking state: \(dock). \(ahead) Guard mode \(guardArmed ? "on" : "off")."
        } catch {
            return friendly(error)
        }
    }
}

struct PatrolTool: Tool {
    let robot: Robot
    let name = "patrol"
    let description = "Run a saved patrol route around the home. Use when asked to patrol, check the house, or do the rounds."

    @Generable
    struct Arguments {
        @Guide(description: "Route name if the person said one, otherwise empty")
        var route: String
    }

    func call(arguments: Arguments) async throws -> String {
        Event.emit("action", "patrol \(arguments.route)")
        do {
            let data = try await robot.api.call("GET", "v1/routes")
            let names = (data["routes"] as? [[String: Any]] ?? []).compactMap { $0["name"] as? String }
            guard !names.isEmpty else { return "I don't have any patrol routes yet. Record one in the Scout app first." }
            let wanted = arguments.route.lowercased()
            let name = names.first { !wanted.isEmpty && ($0.lowercased().contains(wanted) || wanted.contains($0.lowercased())) } ?? names[0]
            let path = "v1/routes/" + (name.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? name) + "/run"
            try await robot.act(path, [:])
            return "Starting my \(name) patrol."
        } catch {
            return friendly(error)
        }
    }
}

struct GuardTool: Tool {
    let robot: Robot
    let name = "guard"
    let description = "Turn guard mode on or off. In guard mode the robot watches for motion, people and pets and records alerts."

    @Generable
    struct Arguments {
        @Guide(description: "on or off", .anyOf(["on", "off"]))
        var state: String
    }

    func call(arguments: Arguments) async throws -> String {
        Event.emit("action", "guard \(arguments.state)")
        do {
            try await robot.act("v1/guard", ["armed": arguments.state == "on"])
            return arguments.state == "on" ? "Guard mode is on. I'll watch for movement." : "Guard mode is off."
        } catch {
            return friendly(error)
        }
    }
}

struct RecordTool: Tool {
    let robot: Robot
    let name = "record"
    let description = "Start or stop recording video and sound to the Mac."

    @Generable
    struct Arguments {
        @Guide(description: "start or stop", .anyOf(["start", "stop"]))
        var action: String
    }

    func call(arguments: Arguments) async throws -> String {
        Event.emit("action", "record \(arguments.action)")
        do {
            try await robot.act("v1/record/\(arguments.action)", [:])
            return arguments.action == "start" ? "Recording." : "Recording saved on the Mac."
        } catch {
            return friendly(error)
        }
    }
}

extension Array {
    subscript(safe i: Int) -> Element? { indices.contains(i) ? self[i] : nil }
}

// MARK: - Speech synthesis to 16 kHz WAV for the robot speaker

enum Speech16k {
    static let synthesizer = AVSpeechSynthesizer()

    static func voice() -> AVSpeechSynthesisVoice? {
        let english = AVSpeechSynthesisVoice.speechVoices().filter { $0.language.hasPrefix("en") }
        return english.max { $0.quality.rawValue < $1.quality.rawValue } ?? AVSpeechSynthesisVoice(language: "en-US")
    }

    static func synthesize(_ text: String) async throws -> Data {
        let utterance = AVSpeechUtterance(string: String(text.prefix(400)))
        utterance.voice = voice()
        let target = AVAudioFormat(commonFormat: .pcmFormatInt16, sampleRate: 16000, channels: 1, interleaved: true)!
        var pcm = Data()
        var converter: AVAudioConverter?
        await withCheckedContinuation { (done: CheckedContinuation<Void, Never>) in
            var finished = false
            synthesizer.write(utterance) { buffer in
                guard let input = buffer as? AVAudioPCMBuffer else { return }
                if input.frameLength == 0 {
                    if !finished { finished = true; done.resume() }
                    return
                }
                if converter == nil { converter = AVAudioConverter(from: input.format, to: target) }
                guard let converter else { return }
                let capacity = AVAudioFrameCount(Double(input.frameLength) * 16000 / input.format.sampleRate) + 64
                guard let out = AVAudioPCMBuffer(pcmFormat: target, frameCapacity: capacity) else { return }
                var supplied = false
                _ = converter.convert(to: out, error: nil) { _, status in
                    if supplied { status.pointee = .noDataNow; return nil }
                    supplied = true
                    status.pointee = .haveData
                    return input
                }
                if let ch = out.int16ChannelData {
                    pcm.append(Data(bytes: ch[0], count: Int(out.frameLength) * 2))
                }
            }
        }
        return wav(pcm)
    }

    static func wav(_ pcm: Data) -> Data {
        var d = Data()
        func u32(_ v: UInt32) { withUnsafeBytes(of: v.littleEndian) { d.append(contentsOf: $0) } }
        func u16(_ v: UInt16) { withUnsafeBytes(of: v.littleEndian) { d.append(contentsOf: $0) } }
        d.append("RIFF".data(using: .ascii)!); u32(UInt32(36 + pcm.count))
        d.append("WAVEfmt ".data(using: .ascii)!); u32(16); u16(1); u16(1); u32(16000); u32(32000); u16(2); u16(16)
        d.append("data".data(using: .ascii)!); u32(UInt32(pcm.count))
        d.append(pcm)
        return d
    }
}

// MARK: - Listening

/// Feeds 16 kHz mono Int16 audio into the speech analyzer, with simple
/// automatic gain because the robot's microphone is quiet.
final class AudioFeed: @unchecked Sendable {
    let input: AsyncStream<AnalyzerInput>.Continuation
    let analyzerFormat: AVAudioFormat
    let source = AVAudioFormat(commonFormat: .pcmFormatInt16, sampleRate: 16000, channels: 1, interleaved: true)!
    var converter: AVAudioConverter?
    var gain: Float = 8
    var muted = false

    init(input: AsyncStream<AnalyzerInput>.Continuation, format: AVAudioFormat) {
        self.input = input
        analyzerFormat = format
        if format != source { converter = AVAudioConverter(from: source, to: format) }
    }

    func push(int16 data: Data) {
        let frames = data.count / 2
        guard frames > 0, let buf = AVAudioPCMBuffer(pcmFormat: source, frameCapacity: AVAudioFrameCount(frames)) else { return }
        buf.frameLength = AVAudioFrameCount(frames)
        let samples = buf.int16ChannelData![0]
        data.withUnsafeBytes { raw in
            let src = raw.bindMemory(to: Int16.self)
            var sumSq: Float = 0
            for i in 0..<frames { let v = Float(Int16(littleEndian: src[i])); sumSq += v * v }
            let rms = (sumSq / Float(frames)).squareRoot()
            // Aim speech at ~3000 RMS; adapt slowly and keep noise from being boosted forever.
            if rms > 30 { gain = gain * 0.9 + min(40, max(1, 3000 / rms)) * 0.1 }
            for i in 0..<frames {
                let v = muted ? 0 : Float(Int16(littleEndian: src[i])) * gain
                samples[i] = Int16(max(-32767, min(32767, v)))
            }
        }
        if let converter {
            let cap = AVAudioFrameCount(Double(frames) * analyzerFormat.sampleRate / 16000) + 64
            guard let out = AVAudioPCMBuffer(pcmFormat: analyzerFormat, frameCapacity: cap) else { return }
            var supplied = false
            _ = converter.convert(to: out, error: nil) { _, status in
                if supplied { status.pointee = .noDataNow; return nil }
                supplied = true
                status.pointee = .haveData
                return buf
            }
            input.yield(AnalyzerInput(buffer: out))
        } else {
            input.yield(AnalyzerInput(buffer: buf))
        }
    }
}

func streamRobotMicrophone(api: ScoutAPI, feed: AudioFeed) async {
    while !Task.isCancelled {
        do {
            var req = api.request("GET", "v1/microphone.pcm")
            req.timeoutInterval = 30
            let (bytes, response) = try await api.session.bytes(for: req)
            guard (response as? HTTPURLResponse)?.statusCode == 200 else { throw APIError(status: 0, message: "microphone unavailable") }
            var chunk = Data()
            chunk.reserveCapacity(3200)
            for try await byte in bytes {
                chunk.append(byte)
                if chunk.count >= 3200 {
                    feed.push(int16: chunk)
                    chunk.removeAll(keepingCapacity: true)
                }
            }
        } catch {
            if Task.isCancelled { return }
            Event.emit("error", "robot microphone: \(friendly(error)); retrying")
            try? await Task.sleep(for: .seconds(3))
        }
    }
}

func streamMacMicrophone(feed: AudioFeed) throws -> AVAudioEngine {
    let engine = AVAudioEngine()
    let input = engine.inputNode
    let format = input.outputFormat(forBus: 0)
    let converter = AVAudioConverter(from: format, to: feed.source)!
    input.installTap(onBus: 0, bufferSize: 4096, format: format) { buffer, _ in
        let cap = AVAudioFrameCount(Double(buffer.frameLength) * 16000 / format.sampleRate) + 64
        guard let out = AVAudioPCMBuffer(pcmFormat: feed.source, frameCapacity: cap) else { return }
        var supplied = false
        _ = converter.convert(to: out, error: nil) { _, status in
            if supplied { status.pointee = .noDataNow; return nil }
            supplied = true
            status.pointee = .haveData
            return buffer
        }
        feed.push(int16: Data(bytes: out.int16ChannelData![0], count: Int(out.frameLength) * 2))
    }
    try engine.start()
    return engine
}

// MARK: - Assistant

let instructions = """
You are Scout, a small wheeled home robot with a camera. You hear spoken commands and act using your tools.
Speak as the robot in the first person ("I'm charging", "I turned left").
- To go forward, backward or sideways use move. To rotate use turn ("turn around" is 180 degrees, "spin in a circle" is 360, "look left" means turn left 90 degrees).
- If the person says stop, call stop.
- To go home, charge, or return to base, call dock.
- For battery, charging, or "is anything in front of you" questions, call status.
- To patrol or check the house, call patrol. To watch for intruders or movement, call guard. To film, call record.
- Only report what tools return. Never pretend to move.
- Reply with one short, friendly spoken sentence. No lists, no emoji.
"""

func normalize(_ s: String) -> String {
    s.lowercased().components(separatedBy: CharacterSet.letters.union(.decimalDigits).union(.whitespaces).inverted).joined()
        .split(separator: " ").joined(separator: " ")
}

/// Returns the command after the wake word, "" if only the wake word was heard, or nil.
func command(after text: String) -> String? {
    let words = normalize(text).split(separator: " ").map(String.init)
    for wake in ["scout", "scott", "scouts"] {
        if let i = words.firstIndex(of: wake) {
            return words[(i + 1)...].joined(separator: " ")
        }
    }
    return nil
}

// MARK: - Fast path for common commands

/// Handles everyday commands directly: faster than the model and never
/// "pretends" to act. Returns the spoken reply, or nil to ask the model.
func quickCommand(_ text: String, robot: Robot) async -> String? {
    let words = normalize(text).split(separator: " ").map(String.init)
    let has = { (w: String) in words.contains(w) }
    let phrase = words.joined(separator: " ")
    let numberWords: [String: Double] = ["one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "ten": 10, "twenty": 20, "thirty": 30,
                                         "forty": 40, "fifty": 50, "sixty": 60, "ninety": 90, "half": 0.5, "quarter": 0.25]
    func amount() -> Double? {
        for w in words {
            if let v = Double(w) ?? numberWords[w] { return v }
        }
        return nil
    }
    let small = has("little") || has("bit") || has("slightly") || has("tiny")
    func distance() -> Double {
        if small && amount() == nil { return 0.15 }
        let metric = has("meter") || has("meters") || has("metre") || has("metres")
        let cm = has("centimeters") || has("centimetres") || has("cm")
        let feet = has("feet") || has("foot")
        var v = amount() ?? (has("a") && (metric || feet) ? 1 : 0.3)
        if cm || (!metric && !feet && v > 5) { v /= 100 } else if feet { v *= 0.3 }
        return min(2, max(0.05, v))
    }
    if words.count <= 3 && (has("stop") || has("halt") || has("freeze")) {
        _ = try? await StopTool(robot: robot).call(arguments: .init())
        return "Stopping."
    }
    if phrase.contains("circle") || phrase.contains("all the way around") || phrase.contains("full turn") || phrase.contains("360") {
        return try? await TurnTool(robot: robot).call(arguments: .init(direction: has("right") ? "right" : "left", degrees: 360))
    }
    if has("look") && (has("left") || has("right")) && !has("go") && !has("move") {
        return try? await TurnTool(robot: robot).call(arguments: .init(direction: has("left") ? "left" : "right", degrees: small ? 30 : 90))
    }
    if phrase.contains("turn around") || phrase.contains("about face") {
        return try? await TurnTool(robot: robot).call(arguments: .init(direction: "left", degrees: 180))
    }
    if has("turn") || has("rotate") || has("spin") {
        guard has("left") || has("right") else { return nil }
        var degrees = Int(amount() ?? 90)
        if small && amount() == nil { degrees = 30 }
        return try? await TurnTool(robot: robot).call(arguments: .init(direction: has("left") ? "left" : "right", degrees: min(360, max(5, degrees))))
    }
    let go = has("go") || has("move") || has("drive") || has("come") || has("back") || has("forward")
    if go && (has("forward") || has("ahead") || has("forwards")) && !has("left") && !has("right") {
        return try? await MoveTool(robot: robot).call(arguments: .init(direction: "forward", meters: distance()))
    }
    if go && (has("backward") || has("backwards") || phrase.contains("back up") || phrase.contains("go back")) && !has("home") && !has("charger") && !has("dock") {
        return try? await MoveTool(robot: robot).call(arguments: .init(direction: "backward", meters: distance()))
    }
    if has("patrol") || phrase.contains("do the rounds") || phrase.contains("check the house") {
        return try? await PatrolTool(robot: robot).call(arguments: .init(route: ""))
    }
    if (has("record") || has("recording") || has("film")) && !has("route") {
        return try? await RecordTool(robot: robot).call(arguments: .init(action: has("stop") || has("end") ? "stop" : "start"))
    }
    if has("guard") || phrase.contains("keep watch") || phrase.contains("watch the house") {
        return try? await GuardTool(robot: robot).call(arguments: .init(state: has("off") || has("stop") || has("disarm") ? "off" : "on"))
    }
    if has("dock") || phrase.contains("go home") || phrase.contains("charge") || has("charger") {
        return try? await DockTool(robot: robot).call(arguments: .init())
    }
    if has("light") || has("lights") {
        if has("brighter") || has("on") || has("up") { return try? await LightTool(robot: robot).call(arguments: .init(setting: "brighter")) }
        if has("dimmer") || has("off") || has("down") { return try? await LightTool(robot: robot).call(arguments: .init(setting: "dimmer")) }
        if has("auto") || has("automatic") { return try? await LightTool(robot: robot).call(arguments: .init(setting: "automatic")) }
    }
    return nil
}

@main
struct ScoutVoice {
    static func main() async {
        let args = CommandLine.arguments
        func value(_ flag: String) -> String? { args.firstIndex(of: flag).flatMap { $0 + 1 < args.count ? args[$0 + 1] : nil } }
        do {
            try await run(macMicrophone: value("--source") == "mac", text: value("--text"), dryRun: args.contains("--dry-run"))
        } catch {
            Event.emit("fatal", "\(error)")
            exit(1)
        }
    }

    static func run(macMicrophone: Bool, text: String?, dryRun: Bool) async throws {
        let model = SystemLanguageModel.default
        guard case .available = model.availability else {
            throw APIError(status: 0, message: "Apple's on-device model is unavailable (\(model.availability)). Turn on Apple Intelligence in System Settings.")
        }
        let api = try ScoutAPI()
        let robot = Robot(api: api, dryRun: dryRun)
        let tools: [any Tool] = [MoveTool(robot: robot), TurnTool(robot: robot), StopTool(robot: robot),
                                 DockTool(robot: robot), LightTool(robot: robot), StatusTool(robot: robot),
                                 PatrolTool(robot: robot), GuardTool(robot: robot), RecordTool(robot: robot)]
        if let text {
            // One command without listening, for testing.
            let session = LanguageModelSession(tools: tools, instructions: instructions)
            Event.emit("heard", text)
            let reply: String
            if let quick = await quickCommand(text, robot: robot) {
                reply = quick
            } else {
                reply = try await session.respond(to: text).content
            }
            Event.emit("reply", reply)
            if !dryRun { await robot.say(reply) }
            return
        }

        // Speech recognition (on-device).
        guard let locale = await SpeechTranscriber.supportedLocale(equivalentTo: Locale(identifier: "en-US")) else {
            throw APIError(status: 0, message: "English speech recognition is not supported on this Mac")
        }
        let transcriber = SpeechTranscriber(locale: locale, transcriptionOptions: [], reportingOptions: [], attributeOptions: [])
        if let install = try await AssetInventory.assetInstallationRequest(supporting: [transcriber]) {
            Event.emit("status", "Downloading the speech model…")
            try await install.downloadAndInstall()
        }
        let analyzer = SpeechAnalyzer(modules: [transcriber])
        guard let format = await SpeechAnalyzer.bestAvailableAudioFormat(compatibleWith: [transcriber]) else {
            throw APIError(status: 0, message: "no audio format for speech recognition")
        }
        let (stream, continuation) = AsyncStream<AnalyzerInput>.makeStream()
        let feed = AudioFeed(input: continuation, format: format)
        try await analyzer.start(inputSequence: stream)

        var engine: AVAudioEngine?
        var micTask: Task<Void, Never>?
        if macMicrophone {
            engine = try streamMacMicrophone(feed: feed)
        } else {
            micTask = Task { await streamRobotMicrophone(api: api, feed: feed) }
        }
        _ = engine

        var session = LanguageModelSession(tools: tools, instructions: instructions)
        session.prewarm()
        var awaitingUntil = Date.distantPast
        Event.emit("listening")

        for try await result in transcriber.results {
            let text = String(result.text.characters).trimmingCharacters(in: .whitespacesAndNewlines)
            guard !text.isEmpty, !feed.muted else { continue }
            let normalized = normalize(text)
            // "Stop" always works while the robot is moving, without the wake word.
            if await robot.moving, normalized.split(separator: " ").contains("stop") {
                await robot.stop()
                Event.emit("action", "stop")
                continue
            }
            var request: String?
            if let after = command(after: text) {
                if after.isEmpty {
                    awaitingUntil = Date().addingTimeInterval(8)
                    Event.emit("wake")
                    continue
                }
                request = after
            } else if Date() < awaitingUntil {
                request = normalized
            }
            guard let request else { continue }
            awaitingUntil = .distantPast
            Event.emit("heard", request)
            Event.emit("thinking")
            var reply: String
            do {
                if let quick = await quickCommand(request, robot: robot) {
                    reply = quick
                } else {
                    reply = try await session.respond(to: request).content
                }
            } catch LanguageModelSession.GenerationError.exceededContextWindowSize {
                session = LanguageModelSession(tools: tools, instructions: instructions)
                reply = (try? await session.respond(to: request).content) ?? "Sorry, could you say that again?"
            } catch {
                Event.emit("error", "model: \(error)")
                reply = "Sorry, I couldn't do that."
            }
            reply = reply.trimmingCharacters(in: .whitespacesAndNewlines)
            if reply.isEmpty { continue }
            feed.muted = true
            await robot.say(reply)
            try? await Task.sleep(for: .milliseconds(Int(Double(reply.count) * 65) + 800))
            feed.muted = false
        }
        micTask?.cancel()
    }
}
