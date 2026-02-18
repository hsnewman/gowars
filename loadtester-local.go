//go:build loadtester
// +build loadtester

package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"math/rand"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	targetAddr           = "localhost:1701"
	connectionsPerGalaxy = 16
	totalGalaxies        = 4
	sendDelay            = 500 * time.Millisecond

	// promptIdleTimeout is the maximum time a worker will wait without receiving any bytes
	// while waiting for a prompt. If exceeded, the worker sends an empty line ("<enter>")
	// and continues waiting.
	promptIdleTimeout = 5 * time.Second

	// debugIO enables verbose logging of every network read/write and prompt wait.
	// WARNING: this is extremely noisy with many connections.
	debugIO = true

	// debugIOMaxPreview limits how much of a payload is printed (still logs byte count + sha256).
	debugIOMaxPreview = 400

	// debugIOTruncateAccumulatedPreview limits how much of the accumulated buffer we log on prompt checks.
	debugIOTruncateAccumulatedPreview = 300
)

// Entity represents a parsed object from the game output, such as a base or ship.
type Entity struct {
	Name   string // e.g., "Fed Base", "Goblin"
	Type   string // e.g., "Base", "Ship"
	X, Y   int    // Absolute coordinates (if available)
	DX, DY int    // Relative coordinates (if available)
	Status string // e.g., "-93%"
	Raw    string // The raw line from which this entity was parsed
}

// CommandContext holds the current parsed state from previous commands.
type CommandContext struct {
	Entities      []Entity
	LastRawOutput string
	ShipName      string // Name of our ship
	ShipX, ShipY  int    // Our ship's position
	Galaxy        int    // Galaxy number we're in
}

// NewCommandContext creates a new, empty CommandContext.
func NewCommandContext(galaxy int) *CommandContext {
	return &CommandContext{
		Entities:      []Entity{},
		LastRawOutput: "",
		Galaxy:        galaxy,
	}
}

// entityRegex matches lines like: "*Fed Base        @37-17   +0,-4    -100%"
var entityRegex = regexp.MustCompile(`\*([^@]+)\s+@(\d+)-(\d+)\s+([+-]?\d+),([+-]?\d+)\s+(-?\d+%)`)

// ParseEntities parses game output and returns a slice of Entities found.
func ParseEntities(output string) []Entity {
	lines := strings.Split(output, "\n")
	var entities []Entity

	for _, line := range lines {
		m := entityRegex.FindStringSubmatch(line)
		if m != nil {
			x := atoi(m[2])
			y := atoi(m[3])
			dx := atoi(m[4])
			dy := atoi(m[5])
			name := strings.TrimSpace(m[1])
			typ := inferType(name)
			status := m[6]
			entity := Entity{
				Name:   name,
				Type:   typ,
				X:      x,
				Y:      y,
				DX:     dx,
				DY:     dy,
				Status: status,
				Raw:    line,
			}
			entities = append(entities, entity)
		}
	}
	return entities
}

// inferType tries to guess the entity type from its name.
func inferType(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	switch {
	case strings.Contains(name, "base"):
		return "Base"
	case strings.Contains(name, "ship"):
		return "Ship"
	default:
		return "Unknown"
	}
}

// atoi is a helper to parse int, returns 0 on error.
func atoi(s string) int {
	i, _ := strconv.Atoi(s)
	return i
}

// parseShipInfo extracts ship name and position from game output
func parseShipInfo(output string) (shipName string, x, y int, found bool) {
	lines := strings.Split(output, "\n")
	// Look for lines that might contain ship info
	for _, line := range lines {
		// Look for patterns like "Ship: ShipName at 37-17"
		if strings.Contains(line, "Ship:") {
			parts := strings.Fields(line)
			for i, part := range parts {
				if part == "Ship:" && i+1 < len(parts) {
					shipName = parts[i+1]
					// Look for coordinates in the line
					coordRegex := regexp.MustCompile(`(\d+)-(\d+)`)
					if match := coordRegex.FindStringSubmatch(line); match != nil {
						x = atoi(match[1])
						y = atoi(match[2])
						found = true
						return
					}
				}
			}
		}
		// Alternative pattern: look for our ship in entity list (marked differently)
		if strings.HasPrefix(line, " ") && strings.Contains(line, "@") {
			// This might be our ship (without asterisk)
			entityRegex := regexp.MustCompile(`^\s*([^@]+)\s+@(\d+)-(\d+)`)
			if match := entityRegex.FindStringSubmatch(line); match != nil {
				shipName = strings.TrimSpace(match[1])
				x = atoi(match[2])
				y = atoi(match[3])
				found = true
				return
			}
		}
	}
	return "", 0, 0, false
}

// dbgPreview returns a safe-ish preview plus metadata for debug logs.
func dbgPreview(s string, max int) (preview string, bytes int, sha string) {
	b := []byte(s)
	sum := sha256.Sum256(b)
	sha = hex.EncodeToString(sum[:])
	bytes = len(b)

	if max <= 0 || len(s) <= max {
		return s, bytes, sha
	}
	return s[:max] + fmt.Sprintf("...(truncated, total=%d bytes)", bytes), bytes, sha
}

func dbgLogf(galaxy int, format string, args ...any) {
	if !debugIO {
		return
	}
	timestamp := time.Now().Format("15:04:05.000")
	prefix := fmt.Sprintf("[%s][Galaxy %d][IO] ", timestamp, galaxy)
	log.Printf(prefix+format, args...)
}

func dbgLogWrite(galaxy int, label string, payload []byte, n int, err error) {
	if !debugIO {
		return
	}
	preview, bytes, sha := dbgPreview(string(payload), debugIOMaxPreview)
	dbgLogf(galaxy, "WRITE %s: wrote=%d/%d err=%v sha256=%s payload=%q", label, n, bytes, err, sha, preview)
}

func dbgLogRead(galaxy int, label string, chunk []byte, n int, err error) {
	if !debugIO {
		return
	}
	preview, bytes, sha := dbgPreview(string(chunk[:n]), debugIOMaxPreview)
	dbgLogf(galaxy, "READ  %s: read=%d/%d err=%v sha256=%s chunk=%q", label, n, bytes, err, sha, preview)
}

// writeLine writes a command line and logs the outgoing payload and result.
func writeLine(galaxy int, conn net.Conn, label, line string) error {
	payload := []byte(line + "\n")
	n, err := conn.Write(payload)
	dbgLogWrite(galaxy, label, payload, n, err)
	return err
}

// logCommand logs a command with context information and timestamp
func logCommand(galaxy int, ctx *CommandContext, command, description string) {
	shipInfo := ""
	if ctx.ShipName != "" {
		shipInfo = fmt.Sprintf(" [Ship: %s at %d-%d]", ctx.ShipName, ctx.ShipX, ctx.ShipY)
	}

	entityCount := len(ctx.Entities)
	baseCount := 0
	shipCount := 0
	for _, e := range ctx.Entities {
		if e.Type == "Base" {
			baseCount++
		} else if e.Type == "Ship" {
			shipCount++
		}
	}

	timestamp := time.Now().Format("15:04:05.000")
	log.Printf("[%s][Galaxy %d]%s Command: '%s' | %s | Entities: %d (%d bases, %d ships)",
		timestamp, galaxy, shipInfo, command, description, entityCount, baseCount, shipCount)
}

// logCommandResult logs the result of a command execution
func logCommandResult(galaxy int, command, result string, duration time.Duration) {
	timestamp := time.Now().Format("15:04:05.000")
	log.Printf("[%s][Galaxy %d] Result for '%s' (took %dms): %s",
		timestamp, galaxy, command, duration.Milliseconds(), result)
}

func main() {
	fmt.Printf("Starting load test: %d total connections...\n", totalGalaxies*connectionsPerGalaxy)

	rand.Seed(time.Now().UnixNano())

	for galaxy := 0; galaxy < totalGalaxies; galaxy++ {
		for i := 0; i < connectionsPerGalaxy; i++ {
			go startWorker(galaxy)
		}
	}

	select {}
}

func startWorker(galaxy int) {
	ticker := time.NewTicker(500 * time.Millisecond) // Rate limit to 2 commands per second
	defer ticker.Stop()
	log.Printf("[Galaxy %d] Connecting to %s...", galaxy, targetAddr)
	conn, err := net.Dial("tcp", targetAddr)
	if err != nil {
		log.Printf("[Galaxy %d] Connection failed: %v", galaxy, err)
		return
	}
	defer conn.Close()
	log.Printf("[Galaxy %d] Connected successfully", galaxy)

	reader := bufio.NewReader(conn)

	// 1. Wait for the pregame prompt (new servers show "PG>", older tooling used "pregame>")
	log.Printf("[Galaxy %d] Waiting for pregame prompt (PG> / pregame>)...", galaxy)
	if !waitForPrompt(galaxy, conn, reader, "PG>") && !waitForPrompt(galaxy, conn, reader, "pregame>") {
		log.Printf("[Galaxy %d] Did not receive pregame prompt", galaxy)
		return
	}
	log.Printf("[Galaxy %d] Received pregame prompt", galaxy)

	// 2. Join the galaxy - make it a gowars game so we can test at baud=0
	joinCmd := fmt.Sprintf("a %d False 50 50", galaxy)
	log.Printf("[Galaxy %d] Joining galaxy with command: %s", galaxy, joinCmd)
	if err := writeLine(galaxy, conn, "join", joinCmd); err != nil {
		log.Printf("[Galaxy %d] Write error (join): %v", galaxy, err)
		return
	}

	// 3. Wait for in-game prompt (e.g., "Command:")
	log.Printf("[Galaxy %d] Waiting for Command prompt...", galaxy)
	if !waitForPrompt(galaxy, conn, reader, "Command:") {
		log.Printf("[Galaxy %d] Did not receive Command prompt", galaxy)
		return
	}
	log.Printf("[Galaxy %d] Successfully joined galaxy and ready for commands", galaxy)

	ctx := NewCommandContext(galaxy)

	for {
		// 1. Wait for the ticker to allow the next command
		<-ticker.C

		// 2. Wait for prompt before sending scan command
		if !waitForPrompt(galaxy, conn, reader, "Command:") {
			log.Printf("[Galaxy %d] Lost connection or prompt not received before scan", galaxy)
			return
		}

		// 2. Send scan command
		command := "li cl en"
		startTime := time.Now()
		logCommand(galaxy, ctx, command, "Scanning for entities")
		if err := writeLine(galaxy, conn, "scan", command); err != nil {
			log.Printf("[Galaxy %d] Write error (%s): %v", galaxy, command, err)
			return
		}
		output := readUntilPrompt(galaxy, conn, reader, "Command:", "scan")
		scanDuration := time.Since(startTime)
		ctx.LastRawOutput = output
		oldEntityCount := len(ctx.Entities)
		ctx.Entities = ParseEntities(output)

		// Log entity parsing results
		scanResult := fmt.Sprintf("Found %d entities", len(ctx.Entities))
		if len(ctx.Entities) != oldEntityCount {
			scanResult = fmt.Sprintf("Found %d entities (was %d)", len(ctx.Entities), oldEntityCount)
			for _, entity := range ctx.Entities {
				timestamp := time.Now().Format("15:04:05.000")
				log.Printf("[%s][Galaxy %d] Entity: %s (%s) at %d-%d (rel: %+d,%+d) %s",
					timestamp, galaxy, entity.Name, entity.Type, entity.X, entity.Y,
					entity.DX, entity.DY, entity.Status)
			}
		}
		logCommandResult(galaxy, command, scanResult, scanDuration)

		// Update ship information from output
		if shipName, x, y, found := parseShipInfo(output); found {
			if ctx.ShipName != shipName || ctx.ShipX != x || ctx.ShipY != y {
				timestamp := time.Now().Format("15:04:05.000")
				log.Printf("[%s][Galaxy %d] Ship info updated: %s at %d-%d (was %s at %d-%d)",
					timestamp, galaxy, shipName, x, y, ctx.ShipName, ctx.ShipX, ctx.ShipY)
			}
			ctx.ShipName = shipName
			ctx.ShipX = x
			ctx.ShipY = y
		}

		// Find the nearest enemy (ship or base, but not our own ship)
		var target *Entity
		minDist := 99999
		for i, e := range ctx.Entities {
			if (e.Type == "Ship" || e.Type == "Base") && e.Name != ctx.ShipName {
				dist := abs(e.DX) + abs(e.DY)
				if dist < minDist {
					minDist = dist
					target = &ctx.Entities[i]
				}
			}
		}

		if target != nil {
			timestamp := time.Now().Format("15:04:05.000")
			log.Printf("[%s][Galaxy %d] Nearest target: %s (%s) at %d-%d (rel: %+d,%+d), dist=%d",
				timestamp, galaxy, target.Name, target.Type, target.X, target.Y, target.DX, target.DY, minDist)
		} else {
			timestamp := time.Now().Format("15:04:05.000")
			log.Printf("[%s][Galaxy %d] No enemy ships or bases found in scan", timestamp, galaxy)
		}

		// 3. If a target is found and in range, attack (range <= 3)
		if target != nil && minDist <= 3 {
			// Wait for the ticker to allow the next command
			<-ticker.C

			if !waitForPrompt(galaxy, conn, reader, "Command:") {
				log.Printf("[Galaxy %d] Lost connection or prompt not received before weapon command", galaxy)
				return
			}
			weaponType := "torpedo"
			weaponCmd := fmt.Sprintf("tor 3 %d %d", target.DX, target.DY)
			description := fmt.Sprintf("Firing torpedo at %s (%s) (rel: %+d,%+d, abs: %d-%d)",
				target.Name, target.Type, target.DX, target.DY, target.X, target.Y)
			if rand.Intn(2) == 0 {
				weaponType = "phasers"
				weaponCmd = fmt.Sprintf("phasers %d %d", target.DX, target.DY)
				description = fmt.Sprintf("Firing phasers at %s (%s) (rel: %+d,%+d, abs: %d-%d)",
					target.Name, target.Type, target.DX, target.DY, target.X, target.Y)
			}
			weaponStart := time.Now()
			logCommand(galaxy, ctx, weaponCmd, description)
			if err := writeLine(galaxy, conn, "weapon", weaponCmd); err != nil {
				log.Printf("[Galaxy %d] Write error (%s): %v", galaxy, weaponType, err)
				return
			}
			weaponOutput := readUntilPrompt(galaxy, conn, reader, "Command:", "weapon")
			weaponDuration := time.Since(weaponStart)

			// Log the result of the weapon command
			var result string
			lowerOutput := strings.ToLower(weaponOutput)
			if weaponType == "torpedo" {
				if strings.Contains(lowerOutput, "hit") {
					result = fmt.Sprintf("✓ HIT on %s!", target.Name)
				} else if strings.Contains(lowerOutput, "miss") {
					result = fmt.Sprintf("✗ Missed %s", target.Name)
				} else if strings.Contains(lowerOutput, "destroyed") {
					result = fmt.Sprintf("💥 DESTROYED %s!", target.Name)
				} else {
					result = fmt.Sprintf("Unclear result for %s", target.Name)
				}
			} else { // phasers
				if strings.Contains(lowerOutput, "hit") {
					result = fmt.Sprintf("✓ PHASERS HIT %s!", target.Name)
				} else if strings.Contains(lowerOutput, "miss") {
					result = fmt.Sprintf("✗ PHASERS missed %s!", target.Name)
				} else if strings.Contains(lowerOutput, "destroyed") {
					result = fmt.Sprintf("💥 PHASERS DESTROYED %s!", target.Name)
				} else {
					result = fmt.Sprintf("Unclear phaser result for %s", target.Name)
				}
			}
			logCommandResult(galaxy, weaponCmd, result, weaponDuration)
		}

		// 4. Wait for prompt before moving
		if !waitForPrompt(galaxy, conn, reader, "Command:") {
			log.Printf("[Galaxy %d] Lost connection or prompt not received before move", galaxy)
			return
		}

		// 5. Move toward the nearest target if one exists and not already adjacent, else random move
		var dx, dy int
		if target != nil && minDist > 1 {
			// Clamp movement to max 6 units per turn
			dx = clamp(target.DX, -6, 6)
			dy = clamp(target.DY, -6, 6)
			moveCmd := fmt.Sprintf("mo %d %d", dx, dy)
			newX, newY := ctx.ShipX+dx, ctx.ShipY+dy
			description := fmt.Sprintf("Moving toward %s from %d-%d to %d-%d (dx: %+d, dy: %+d)", target.Name, ctx.ShipX, ctx.ShipY, newX, newY, dx, dy)
			moveStart := time.Now()
			logCommand(galaxy, ctx, moveCmd, description)
			if err := writeLine(galaxy, conn, "move", moveCmd); err != nil {
				log.Printf("[Galaxy %d] Write error (move): %v", galaxy, err)
				return
			}
			moveOutput := readUntilPrompt(galaxy, conn, reader, "Command:", "move")
			moveDuration := time.Since(moveStart)

			// Update position after move and check for movement confirmation
			var moveResult string
			if strings.Contains(strings.ToLower(moveOutput), "moved") ||
				strings.Contains(strings.ToLower(moveOutput), "now at") {
				moveResult = "✓ Movement confirmed"
			}

			if shipName, x, y, found := parseShipInfo(moveOutput); found {
				if ctx.ShipX != x || ctx.ShipY != y {
					moveResult = fmt.Sprintf("✓ %s moved from %d-%d to %d-%d", shipName, ctx.ShipX, ctx.ShipY, x, y)
				}
				ctx.ShipName = shipName
				ctx.ShipX = x
				ctx.ShipY = y
			} else {
				// If we can't parse position, log the movement attempt
				moveResult = "Movement attempted but position unclear"
			}

			if moveResult != "" {
				logCommandResult(galaxy, moveCmd, moveResult, moveDuration)
			}
		} else {
			// fallback: random move
			dx = rand.Intn(13) - 6 // -6 to 6
			dy = rand.Intn(13) - 6 // -6 to 6
			moveCmd := fmt.Sprintf("mo %d %d", dx, dy)
			newX, newY := ctx.ShipX+dx, ctx.ShipY+dy
			description := fmt.Sprintf("Random move from %d-%d to %d-%d (dx: %+d, dy: %+d)", ctx.ShipX, ctx.ShipY, newX, newY, dx, dy)
			moveStart := time.Now()
			logCommand(galaxy, ctx, moveCmd, description)
			if err := writeLine(galaxy, conn, "move-random", moveCmd); err != nil {
				log.Printf("[Galaxy %d] Write error (move): %v", galaxy, err)
				return
			}
			moveOutput := readUntilPrompt(galaxy, conn, reader, "Command:", "move-random")
			moveDuration := time.Since(moveStart)

			// Update position after move and check for movement confirmation
			var moveResult string
			if strings.Contains(strings.ToLower(moveOutput), "moved") ||
				strings.Contains(strings.ToLower(moveOutput), "now at") {
				moveResult = "✓ Movement confirmed"
			}

			if shipName, x, y, found := parseShipInfo(moveOutput); found {
				if ctx.ShipX != x || ctx.ShipY != y {
					moveResult = fmt.Sprintf("✓ %s moved from %d-%d to %d-%d", shipName, ctx.ShipX, ctx.ShipY, x, y)
				}
				ctx.ShipName = shipName
				ctx.ShipX = x
				ctx.ShipY = y
			} else {
				// If we can't parse position, log the movement attempt
				moveResult = "Movement attempted but position unclear"
			}

			if moveResult != "" {
				logCommandResult(galaxy, moveCmd, moveResult, moveDuration)
			}
		}
		//more waiting for command prompt
		// Wait for the ticker to allow the next command
		<-ticker.C

		if !waitForPrompt(galaxy, conn, reader, "Command:") {
			log.Printf("[Galaxy %d] Lost connection or prompt not received before move", galaxy)
			return
		}

		// 6. Occasionally send a test message to all (1 in 100 chance)
		if rand.Intn(100) == 0 {
			testCmd := "tell all this is a test"
			logCommand(galaxy, ctx, testCmd, "Random test message to all")
			if err := writeLine(galaxy, conn, "tell-all", testCmd); err != nil {
				log.Printf("[Galaxy %d] Write error (test message): %v", galaxy, err)
				return
			}
			_ = readUntilPrompt(galaxy, conn, reader, "Command:", "tell-all")
			logCommandResult(galaxy, testCmd, "Sent test message", 0)
		}
		//more waiting for command prompt
		// Wait for the ticker to allow the next command
		<-ticker.C

		if !waitForPrompt(galaxy, conn, reader, "Command:") {
			log.Printf("[Galaxy %d] Lost connection or prompt not received before move", galaxy)
			return
		}

	}
}

// waitForPrompt reads the stream until the target string is found.
// Idle "<enter>" behavior is only enabled when waiting for the in-game "Command:" prompt.
func waitForPrompt(galaxy int, conn net.Conn, reader *bufio.Reader, prompt string) bool {
	buffer := make([]byte, 1024)
	accumulated := ""

	dbgLogf(galaxy, "WAIT  prompt=%q (enter)", prompt)

	for {
		// Set a per-iteration read deadline so we can periodically "poke" with <enter>.
		if prompt == "Command:" {
			_ = conn.SetReadDeadline(time.Now().Add(promptIdleTimeout))
		} else {
			_ = conn.SetReadDeadline(time.Time{})
		}

		n, err := reader.Read(buffer)
		dbgLogRead(galaxy, "waitForPrompt", buffer, n, err)

		if err != nil {
			// If we just timed out waiting for bytes while waiting for "Command:", send "<enter>" and try again.
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if prompt == "Command:" {
					dbgLogf(galaxy, "WAIT  prompt=%q idle=%s -> sending <enter>", prompt, promptIdleTimeout)

					before := time.Now()
					if err := writeLine(galaxy, conn, "idle-enter", ""); err != nil {
						dbgLogf(galaxy, "WAIT  prompt=%q (exit=false, idle-enter write err=%v)", prompt, err)
						return false
					}
					dbgLogf(galaxy, "WAIT  prompt=%q idle-enter sent (dt=%s)", prompt, time.Since(before))

					continue
				}
			}

			dbgLogf(galaxy, "WAIT  prompt=%q (exit=false, err=%v)", prompt, err)
			return false
		}

		// Clear deadline once we get data so other reads can block unless they manage their own deadlines.
		_ = conn.SetReadDeadline(time.Time{})

		accumulated += string(buffer[:n])

		if debugIO {
			preview, bytes, sha := dbgPreview(accumulated, debugIOTruncateAccumulatedPreview)
			dbgLogf(galaxy, "WAIT  prompt=%q accumulated=%d sha256=%s preview=%q", prompt, bytes, sha, preview)
		}

		if strings.Contains(accumulated, prompt) {
			dbgLogf(galaxy, "WAIT  prompt=%q (exit=true)", prompt)
			return true
		}

		if len(accumulated) > 5000 {
			accumulated = accumulated[2500:]
		}
	}
}

// readUntilPrompt reads from the reader until the prompt is found and returns all output up to and including the prompt.
// NOTE: Idle "<enter>" behavior is only enabled when waiting for the in-game "Command:" prompt.
func readUntilPrompt(galaxy int, conn net.Conn, reader *bufio.Reader, prompt string, label string) string {
	buffer := make([]byte, 1024)
	accumulated := ""

	dbgLogf(galaxy, "READUNTIL label=%s prompt=%q (enter)", label, prompt)

	for {
		if prompt == "Command:" {
			_ = conn.SetReadDeadline(time.Now().Add(promptIdleTimeout))
		} else {
			_ = conn.SetReadDeadline(time.Time{})
		}

		n, err := reader.Read(buffer)
		dbgLogRead(galaxy, "readUntilPrompt/"+label, buffer, n, err)
		if err != nil {
			// If we just timed out waiting for bytes while waiting for "Command:", send "<enter>" and try again.
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if prompt == "Command:" {
					dbgLogf(galaxy, "READUNTIL label=%s prompt=%q idle=%s -> sending <enter>", label, prompt, promptIdleTimeout)

					before := time.Now()
					if werr := writeLine(galaxy, conn, "idle-enter/"+label, ""); werr != nil {
						if debugIO {
							preview, bytes, sha := dbgPreview(accumulated, debugIOMaxPreview)
							dbgLogf(galaxy, "READUNTIL label=%s prompt=%q (exit=err=%v accumulated=%d sha256=%s preview=%q)",
								label, prompt, werr, bytes, sha, preview)
						}
						return accumulated
					}
					dbgLogf(galaxy, "READUNTIL label=%s prompt=%q idle-enter sent (dt=%s)", label, prompt, time.Since(before))

					continue
				}
			}

			if debugIO {
				preview, bytes, sha := dbgPreview(accumulated, debugIOMaxPreview)
				dbgLogf(galaxy, "READUNTIL label=%s prompt=%q (exit=err=%v accumulated=%d sha256=%s preview=%q)",
					label, prompt, err, bytes, sha, preview)
			}
			return accumulated
		}

		// Clear deadline once we get data so other reads can block unless they manage their own deadlines.
		_ = conn.SetReadDeadline(time.Time{})

		accumulated += string(buffer[:n])

		if strings.Contains(accumulated, prompt) {
			if debugIO {
				preview, bytes, sha := dbgPreview(accumulated, debugIOMaxPreview)
				dbgLogf(galaxy, "READUNTIL label=%s prompt=%q (exit=found accumulated=%d sha256=%s preview=%q)",
					label, prompt, bytes, sha, preview)
			}
			return accumulated
		}

		if len(accumulated) > 10000 {
			accumulated = accumulated[5000:]
		}
	}
}

// abs returns the absolute value of an integer.
func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// clamp restricts x to the range [min, max].
func clamp(x, min, max int) int {
	if x < min {
		return min
	}
	if x > max {
		return max
	}
	return x
}
