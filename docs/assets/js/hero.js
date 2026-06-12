// Hero scene: the wire.
//
// Tool calls flow as light pulses along faint lanes that converge on the
// gateway — a glowing ring portal on the right. Just before the gate each
// pulse meets the amber approval perimeter: most pass straight through
// (auto-approved by policy), some hold amber, then continue green
// (allowed) or bounce back red (denied). A slow mission-control grid
// floor and drifting dust give depth; scene fog fades everything into the
// page background color so the scene reads as part of the page rather
// than a poster pasted behind it. All structure lives in the lower right
// so the hero copy sits over near-empty space.
//
// Engineering guards: no-WebGL → CSS gradient fallback stays visible;
// prefers-reduced-motion → a single static frame; hidden tab → paused
// loop; DPR capped; counts scale down on small screens.

import * as THREE from '../vendor/three.module.min.js';

const canvas = document.getElementById('hero-canvas');
if (canvas) init(canvas);

function init(canvas) {
  let renderer;
  try {
    renderer = new THREE.WebGLRenderer({
      canvas,
      antialias: true,
      alpha: true,
      powerPreference: 'low-power',
    });
  } catch {
    canvas.style.display = 'none'; // CSS .hero-fallback takes over
    return;
  }

  const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  const small = window.matchMedia('(max-width: 720px)').matches;

  const COLOR = {
    green: new THREE.Color('#34d27b'),
    amber: new THREE.Color('#e3a93c'),
    red: new THREE.Color('#f0564f'),
  };

  const scene = new THREE.Scene();
  // Fog toward the page background — the blending mechanism. Everything
  // distant dissolves into #07090d, so the scene has no hard edges.
  scene.fog = new THREE.Fog(0x07090d, 22, 56);

  const camera = new THREE.PerspectiveCamera(48, 1, 0.1, 120);
  camera.position.set(0, 1.9, 24);

  const world = new THREE.Group();
  scene.add(world);

  // ---- glow sprite textures (radial gradients drawn once) -------------
  function glowTexture(hex) {
    const c = document.createElement('canvas');
    c.width = c.height = 64;
    const g = c.getContext('2d');
    const grad = g.createRadialGradient(32, 32, 0, 32, 32, 32);
    grad.addColorStop(0, hex);
    grad.addColorStop(0.4, hex + '55');
    grad.addColorStop(1, 'transparent');
    g.fillStyle = grad;
    g.fillRect(0, 0, 64, 64);
    return new THREE.CanvasTexture(c);
  }
  const TEX = {
    green: glowTexture('#34d27b'),
    amber: glowTexture('#e3a93c'),
    red: glowTexture('#f0564f'),
    white: glowTexture('#9fe8c0'),
  };

  function glowSprite(tex, size, opacity = 1) {
    const m = new THREE.SpriteMaterial({
      map: tex,
      transparent: true,
      opacity,
      blending: THREE.AdditiveBlending,
      depthWrite: false,
    });
    const s = new THREE.Sprite(m);
    s.scale.setScalar(size);
    return s;
  }

  function circleGeometry(r, n) {
    const pts = [];
    for (let i = 0; i < n; i++) {
      const a = (i / n) * Math.PI * 2;
      pts.push(new THREE.Vector3(Math.cos(a) * r, Math.sin(a) * r, 0));
    }
    return new THREE.BufferGeometry().setFromPoints(pts);
  }

  // ---- grid floor -------------------------------------------------------
  // Mission-control ground plane, scrolling almost imperceptibly toward
  // the camera. Fog swallows the far edge.
  const GRID = { spacing: 2.4, halfX: 50, zNear: 12, zFar: -58, y: -4.6 };
  const grid = (() => {
    const pts = [];
    for (let x = -GRID.halfX; x <= GRID.halfX; x += GRID.spacing) {
      pts.push(x, GRID.y, GRID.zNear, x, GRID.y, GRID.zFar);
    }
    for (let z = GRID.zNear; z >= GRID.zFar; z -= GRID.spacing) {
      pts.push(-GRID.halfX, GRID.y, z, GRID.halfX, GRID.y, z);
    }
    const geo = new THREE.BufferGeometry();
    geo.setAttribute('position', new THREE.BufferAttribute(new Float32Array(pts), 3));
    const g = new THREE.LineSegments(geo, new THREE.LineBasicMaterial({
      color: 0x1a2a22, transparent: true, opacity: 0.55,
      blending: THREE.AdditiveBlending, depthWrite: false,
    }));
    world.add(g);
    return g;
  })();

  // ---- gateway ------------------------------------------------------------
  // A ring portal, face angled toward the incoming lanes. Echo rings
  // recede behind it — a hint of tunnel — and a soft core glow breathes.
  const GATE_POS = new THREE.Vector3(10.5, 0.7, -6);
  const GATE_R = 3.3;
  const gate = new THREE.Group();
  gate.position.copy(GATE_POS);
  gate.rotation.set(0.07, -1.02, 0);
  world.add(gate);
  // Unit vector out of the gate's face, toward incoming traffic.
  const gateAxis = new THREE.Vector3(0, 0, 1).applyEuler(gate.rotation).normalize();

  const ringOuter = new THREE.LineLoop(
    circleGeometry(GATE_R, 96),
    new THREE.LineBasicMaterial({
      color: COLOR.green, transparent: true, opacity: 0.3,
      blending: THREE.AdditiveBlending, depthWrite: false,
    })
  );
  gate.add(ringOuter);

  gate.add(new THREE.LineLoop(
    circleGeometry(GATE_R * 0.78, 96),
    new THREE.LineBasicMaterial({
      color: COLOR.green, transparent: true, opacity: 0.1,
      blending: THREE.AdditiveBlending, depthWrite: false,
    })
  ));

  const ringDots = new THREE.Points(
    circleGeometry(GATE_R * 0.9, 42),
    new THREE.PointsMaterial({
      color: COLOR.green, size: 0.09, transparent: true, opacity: 0.5,
      blending: THREE.AdditiveBlending, depthWrite: false,
    })
  );
  gate.add(ringDots);

  // Echo rings receding behind the gate.
  [1.5, 3.0, 4.5].forEach((d, i) => {
    const echo = new THREE.LineLoop(
      circleGeometry(GATE_R * (1 - d * 0.08), 72),
      new THREE.LineBasicMaterial({
        color: COLOR.green, transparent: true, opacity: 0.14 - i * 0.04,
        blending: THREE.AdditiveBlending, depthWrite: false,
      })
    );
    echo.position.z = -d;
    gate.add(echo);
  });

  const coreWire = new THREE.Mesh(
    new THREE.IcosahedronGeometry(1.0, 1),
    new THREE.MeshBasicMaterial({
      color: COLOR.green, wireframe: true, transparent: true, opacity: 0.16,
      blending: THREE.AdditiveBlending, depthWrite: false,
    })
  );
  gate.add(coreWire);

  const coreGlow = glowSprite(TEX.green, 2.6, 0.38);
  gate.add(coreGlow);
  gate.add(glowSprite(TEX.white, 7, 0.13));

  // Amber approval perimeter: a checkpoint ring floating just in front of
  // the gate face — where holding pulses wait for a verdict.
  const HOLD_DIST = 2.4;
  const perimeter = new THREE.Group();
  perimeter.position.z = HOLD_DIST;
  gate.add(perimeter);
  perimeter.add(new THREE.LineLoop(
    circleGeometry(GATE_R * 1.16, 96),
    new THREE.LineBasicMaterial({
      color: COLOR.amber, transparent: true, opacity: 0.16,
      blending: THREE.AdditiveBlending, depthWrite: false,
    })
  ));
  const perimeterDots = new THREE.Points(
    circleGeometry(GATE_R * 1.16, 30),
    new THREE.PointsMaterial({
      color: COLOR.amber, size: 0.09, transparent: true, opacity: 0.45,
      blending: THREE.AdditiveBlending, depthWrite: false,
    })
  );
  perimeter.add(perimeterDots);

  // ---- lanes ----------------------------------------------------------------
  // Faint curves sweeping in from the left edge, low across the screen,
  // converging through the gate and exiting behind it. Pulses ride them.
  const N_LANES = small ? 6 : 10;
  const rand = (lo, hi) => lo + Math.random() * (hi - lo);

  // Basis perpendicular to the gate axis, for spreading lane entry points
  // across the gate's face.
  const up = new THREE.Vector3(0, 1, 0);
  const basisU = new THREE.Vector3().crossVectors(gateAxis, up).normalize();
  const basisV = new THREE.Vector3().crossVectors(basisU, gateAxis).normalize();

  const holdCenter = GATE_POS.clone().addScaledVector(gateAxis, HOLD_DIST);

  const lanes = [];
  for (let i = 0; i < N_LANES; i++) {
    const f = i / Math.max(N_LANES - 1, 1);
    // Entry point on the gate face, spread inside the ring.
    const ja = rand(0, Math.PI * 2);
    const jr = rand(0.1, 0.6) * GATE_R;
    const jitter = basisU.clone().multiplyScalar(Math.cos(ja) * jr)
      .addScaledVector(basisV, Math.sin(ja) * jr);

    const start = new THREE.Vector3(
      rand(-38, -30),
      -4.2 + f * 6.5 + rand(-0.7, 0.7),
      -16 + f * 22 + rand(-2, 2)
    );
    const mid = new THREE.Vector3(
      rand(-10, -4),
      start.y * 0.45 - 0.9,
      start.z * 0.5 - 1
    );
    const front = GATE_POS.clone().addScaledVector(gateAxis, 6).add(jitter);
    const gateP = GATE_POS.clone().add(jitter.clone().multiplyScalar(0.45));
    const exit = GATE_POS.clone().addScaledVector(gateAxis, -6.5)
      .add(jitter.clone().multiplyScalar(0.15));

    const curve = new THREE.CatmullRomCurve3([start, mid, front, gateP, exit]);

    // Find arc-length params where this lane crosses the approval
    // perimeter plane and the gate plane (signed distance sign flip).
    let uHold = 0.74, uGate = 0.86;
    let prevHold = 1, prevGate = 1;
    const tmp = new THREE.Vector3();
    for (let s = 0; s <= 200; s++) {
      const u = s / 200;
      curve.getPointAt(u, tmp);
      const dHold = tmp.clone().sub(holdCenter).dot(gateAxis);
      const dGate = tmp.clone().sub(GATE_POS).dot(gateAxis);
      if (prevHold > 0 && dHold <= 0) uHold = u;
      if (prevGate > 0 && dGate <= 0) uGate = u;
      prevHold = dHold; prevGate = dGate;
    }

    // The visible lane: a faint vertex-faded line. Additive blending means
    // darker vertex colors are effectively more transparent, so the lane
    // dissolves at both ends instead of stopping.
    const SAMPLES = 90;
    const pos = new Float32Array(SAMPLES * 3);
    const col = new Float32Array(SAMPLES * 3);
    for (let s = 0; s < SAMPLES; s++) {
      const u = s / (SAMPLES - 1);
      curve.getPointAt(u, tmp);
      pos[s * 3] = tmp.x; pos[s * 3 + 1] = tmp.y; pos[s * 3 + 2] = tmp.z;
      const fadeIn = Math.min(u / 0.18, 1);
      const fadeOut = Math.min((1 - u) / 0.12, 1);
      const k = 0.16 * fadeIn * fadeOut;
      col[s * 3] = COLOR.green.r * k;
      col[s * 3 + 1] = COLOR.green.g * k;
      col[s * 3 + 2] = COLOR.green.b * k;
    }
    const geo = new THREE.BufferGeometry();
    geo.setAttribute('position', new THREE.BufferAttribute(pos, 3));
    geo.setAttribute('color', new THREE.BufferAttribute(col, 3));
    const line = new THREE.Line(geo, new THREE.LineBasicMaterial({
      vertexColors: true, transparent: true, opacity: 0.85,
      blending: THREE.AdditiveBlending, depthWrite: false,
    }));
    world.add(line);

    lanes.push({ curve, uHold, uGate });
  }

  // ---- dust -------------------------------------------------------------------
  const DUST_COUNT = small ? 110 : 240;
  const dust = (() => {
    const pos = new Float32Array(DUST_COUNT * 3);
    for (let i = 0; i < DUST_COUNT; i++) {
      pos[i * 3] = rand(-34, 44);
      pos[i * 3 + 1] = rand(-5, 9);
      pos[i * 3 + 2] = rand(-44, 12);
    }
    const geo = new THREE.BufferGeometry();
    geo.setAttribute('position', new THREE.BufferAttribute(pos, 3));
    const p = new THREE.Points(geo, new THREE.PointsMaterial({
      color: 0x39434f, size: 0.06, transparent: true, opacity: 0.5,
      depthWrite: false,
    }));
    world.add(p);
    return p;
  })();

  // ---- pulses: tool calls in flight ----------------------------------------
  // Each pulse is a glowing head plus a short vertex-faded trail sampled
  // backward along its lane. Lifecycle: inbound (green) → maybe HOLD at
  // the amber perimeter ("pending") → through the gate (allowed) or
  // reverse back out (red, denied).
  const PULSE_COUNT = small ? 5 : 9;
  const TRAIL_N = 14;
  const TRAIL_SPAN = 0.045; // arc-length param length of the trail

  function makeTrail() {
    const geo = new THREE.BufferGeometry();
    geo.setAttribute('position', new THREE.BufferAttribute(new Float32Array(TRAIL_N * 3), 3));
    geo.setAttribute('color', new THREE.BufferAttribute(new Float32Array(TRAIL_N * 3), 3));
    return new THREE.Line(geo, new THREE.LineBasicMaterial({
      vertexColors: true, transparent: true,
      blending: THREE.AdditiveBlending, depthWrite: false,
    }));
  }

  const pulses = [];
  for (let i = 0; i < PULSE_COUNT; i++) {
    const sprite = glowSprite(TEX.green, 0.55, 0.9);
    const trail = makeTrail();
    world.add(sprite);
    world.add(trail);
    pulses.push({ sprite, trail, state: 'idle', wait: Math.random() * 4 });
  }

  function setPulseColor(p, name) {
    p.sprite.material.map = TEX[name];
    p.sprite.material.needsUpdate = true;
    const c = COLOR[name];
    const col = p.trail.geometry.attributes.color.array;
    for (let i = 0; i < TRAIL_N; i++) {
      const k = Math.pow(1 - i / (TRAIL_N - 1), 1.6); // bright head, dark tail
      col[i * 3] = c.r * k;
      col[i * 3 + 1] = c.g * k;
      col[i * 3 + 2] = c.b * k;
    }
    p.trail.geometry.attributes.color.needsUpdate = true;
  }

  const trailTmp = new THREE.Vector3();
  function updateTrail(p) {
    const pos = p.trail.geometry.attributes.position.array;
    for (let i = 0; i < TRAIL_N; i++) {
      // Tail trails behind the direction of travel.
      const u = Math.min(Math.max(p.u - p.dir * (i / (TRAIL_N - 1)) * TRAIL_SPAN, 0), 1);
      p.lane.curve.getPointAt(u, trailTmp);
      pos[i * 3] = trailTmp.x; pos[i * 3 + 1] = trailTmp.y; pos[i * 3 + 2] = trailTmp.z;
    }
    p.trail.geometry.attributes.position.needsUpdate = true;
  }

  function spawnPulse(p) {
    p.lane = lanes[(Math.random() * lanes.length) | 0];
    const roll = Math.random();
    p.kind = roll < 0.62 ? 'pass' : roll < 0.88 ? 'hold-allow' : 'hold-deny';
    p.state = 'inbound';
    p.dir = 1;
    p.u = 0;
    p.flashed = false;
    p.speed = 1 / (6 + Math.random() * 3); // u per second
    setPulseColor(p, 'green');
    p.sprite.visible = true;
    p.trail.visible = true;
  }

  let gateFlash = 0;

  function updatePulse(p, now, dt) {
    switch (p.state) {
      case 'idle':
        p.wait -= dt;
        p.sprite.visible = false;
        p.trail.visible = false;
        if (p.wait <= 0) spawnPulse(p);
        break;
      case 'inbound': {
        p.u += dt * p.speed;
        if (p.kind !== 'pass' && p.u >= p.lane.uHold) {
          p.state = 'holding';
          p.u = p.lane.uHold;
          p.holdUntil = now + 0.8 + Math.random() * 1.3;
          setPulseColor(p, 'amber');
        } else if (p.u >= 1) {
          finishPulse(p);
          break;
        }
        if (!p.flashed && p.u >= p.lane.uGate) {
          p.flashed = true;
          gateFlash = 1;
        }
        p.sprite.position.copy(p.lane.curve.getPointAt(Math.min(p.u, 1)));
        updateTrail(p);
        break;
      }
      case 'holding': {
        // Pending approval: throb at the perimeter.
        const k = 1 + Math.sin(now * 9) * 0.25;
        p.sprite.scale.setScalar(0.55 * k);
        p.sprite.position.copy(p.lane.curve.getPointAt(p.u));
        updateTrail(p);
        if (now >= p.holdUntil) {
          p.sprite.scale.setScalar(0.55);
          if (p.kind === 'hold-allow') {
            p.state = 'inbound';
            p.kind = 'pass';
            p.speed *= 1.9;
            setPulseColor(p, 'green');
          } else {
            p.state = 'outbound';
            p.dir = -1;
            p.speed *= 1.6;
            p.dieAt = Math.max(p.lane.uHold - 0.3, 0.05);
            setPulseColor(p, 'red');
          }
        }
        break;
      }
      case 'outbound': {
        p.u -= dt * p.speed;
        if (p.u <= p.dieAt) { finishPulse(p); break; }
        p.sprite.position.copy(p.lane.curve.getPointAt(p.u));
        updateTrail(p);
        break;
      }
    }
  }

  function finishPulse(p) {
    p.state = 'idle';
    p.wait = 0.5 + Math.random() * 2.5;
    p.sprite.visible = false;
    p.trail.visible = false;
  }

  // ---- layout / resize -------------------------------------------------------
  const lookTarget = new THREE.Vector3(3.2, 0.3, 0);
  function layout() {
    const w = canvas.clientWidth || canvas.parentElement.clientWidth;
    const h = canvas.clientHeight || canvas.parentElement.clientHeight;
    renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, small ? 1.5 : 2));
    renderer.setSize(w, h, false);
    camera.aspect = w / h;
    camera.updateProjectionMatrix();
    // Wide screens: gate sits right of the copy. Narrow: pulled toward
    // center, scaled down, with the scrim doing the rest.
    if (w > 920) {
      world.position.set(0, 0, 0);
      world.scale.setScalar(1);
      lookTarget.set(3.2, 0.3, 0);
    } else {
      // Sink the gate below the hero copy so its glow never sits behind
      // the paragraph text.
      // Keep the camera target high while sinking the world, so the gate
      // lands in the lower half of the viewport, under the hero copy.
      world.position.set(-6.5, -4.4, 0);
      world.scale.setScalar(0.68);
      lookTarget.set(0, 1.6, 0);
    }
  }
  layout();
  window.addEventListener('resize', layout);

  // ---- pointer parallax -------------------------------------------------------
  const parallax = { x: 0, y: 0, tx: 0, ty: 0 };
  if (!reduceMotion) {
    window.addEventListener('pointermove', (e) => {
      parallax.tx = (e.clientX / window.innerWidth - 0.5) * 2;
      parallax.ty = (e.clientY / window.innerHeight - 0.5) * 2;
    }, { passive: true });
  }

  // ---- frame loop -------------------------------------------------------------
  const clock = new THREE.Clock();
  let running = true;
  document.addEventListener('visibilitychange', () => {
    running = !document.hidden;
    if (running && !reduceMotion) {
      clock.getDelta(); // swallow the gap so pulses don't jump
      requestAnimationFrame(frame);
    }
  });

  function frame() {
    if (!running) return;
    const dt = Math.min(clock.getDelta(), 0.1);
    const now = clock.elapsedTime;

    coreWire.rotation.y += dt * 0.25;
    coreWire.rotation.x += dt * 0.11;
    ringDots.rotation.z += dt * 0.06;
    perimeterDots.rotation.z -= dt * 0.05;

    gateFlash = Math.max(gateFlash - dt * 1.8, 0);
    coreGlow.material.opacity = 0.36 + Math.sin(now * 1.5) * 0.06 + gateFlash * 0.25;
    ringOuter.material.opacity = 0.28 + gateFlash * 0.2;

    // Grid drifts toward the camera, wrapping every cell so the seam is
    // invisible.
    grid.position.z = (now * 0.45) % GRID.spacing;
    dust.position.y = Math.sin(now * 0.07) * 0.5;

    for (const p of pulses) updatePulse(p, now, dt);

    parallax.x += (parallax.tx - parallax.x) * 0.04;
    parallax.y += (parallax.ty - parallax.y) * 0.04;
    camera.position.x = parallax.x * 1.6;
    camera.position.y = 1.9 - parallax.y * 1.2;
    camera.lookAt(lookTarget);

    renderer.render(scene, camera);
    if (!reduceMotion) requestAnimationFrame(frame);
  }

  if (reduceMotion) {
    // Single composed frame: pulses frozen mid-flight, one holding amber.
    const staticU = [0.2, 0.38, 0.55, 0.68, 0.9];
    pulses.forEach((p, i) => {
      if (i >= staticU.length) return;
      p.lane = lanes[i % lanes.length];
      p.dir = 1;
      if (i === 3) {
        p.u = p.lane.uHold;
        setPulseColor(p, 'amber');
      } else {
        p.u = staticU[i];
        setPulseColor(p, 'green');
      }
      p.sprite.position.copy(p.lane.curve.getPointAt(p.u));
      p.sprite.visible = true;
      p.trail.visible = true;
      updateTrail(p);
    });
    camera.lookAt(lookTarget);
    renderer.render(scene, camera);
    // Re-render on resize so the static frame stays crisp.
    window.addEventListener('resize', () => {
      camera.lookAt(lookTarget);
      renderer.render(scene, camera);
    });
  } else {
    requestAnimationFrame(frame);
  }
}
