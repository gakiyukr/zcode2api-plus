const fs = require('fs');
const path = require('path');
const { JSDOM, ResourceLoader, VirtualConsole } = require('jsdom');

const SCENE = process.argv[2] || '11xygtvd';
const REGION = process.argv[3] || 'sgp';
const PREFIX = process.argv[4] || 'no8xfe';
const SDK_PATH = path.join(__dirname, 'AliyunCaptcha.js.txt');
const FAKE_UA = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36';
const SDK_LOAD_TIMEOUT = Number(process.env.ZCODE_CAPTCHA_SDK_LOAD_MS || 20_000);
const SOLVE_TIMEOUT = Number(process.env.ZCODE_CAPTCHA_SOLVE_MS || 40_000);

function safeError(error) {
  const source = error && typeof error === 'object' ? error : { message: String(error) };
  const result = {};
  for (const key of ['verifyCode', 'code', 'name', 'type', 'message']) {
    const value = source[key];
    if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') {
      result[key] = String(value).replace(/[A-Za-z0-9+/=_-]{64,}/g, '[redacted]').slice(0, 240);
    }
  }
  return JSON.stringify(result).slice(0, 600);
}

function captchaError(kind, error, exitCode) {
  const wrapped = new Error(`${kind}: ${safeError(error)}`);
  wrapped.kind = kind;
  wrapped.exitCode = exitCode;
  return wrapped;
}

class FeiLinBlockingLoader extends ResourceLoader {
  fetch(url, options) {
    if (/FeiLin/i.test(url)) {
      return Object.assign(
        Promise.resolve(Buffer.from('window.__zcode_feilin_blocked=true;')),
        { abort() {} },
      );
    }
    return super.fetch(url, options);
  }
}

function applyPolyfills(window) {
  window.matchMedia = () => ({
    matches: false, media: '', onchange: null,
    addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {},
    dispatchEvent() { return false; },
  });
  let rafId = 0;
  window.requestAnimationFrame = (callback) => {
    const id = ++rafId;
    setTimeout(() => callback(Date.now()), 16);
    return id;
  };
  window.cancelAnimationFrame = (id) => clearTimeout(id);

  const proto = window.HTMLCanvasElement.prototype;
  proto.getContext = function getContext(type) {
    if (/webgl/i.test(type)) {
      return {
        canvas: this,
        getParameter(parameter) {
          if (parameter === 37445) return 'Intel Inc.';
          if (parameter === 37446) return 'Intel Iris OpenGL Engine';
          return 'Intel';
        },
        getExtension: () => null,
        getSupportedExtensions: () => ['WEBGL_debug_renderer_info'],
        getContextAttributes: () => ({}),
        getShaderPrecisionFormat: () => ({ precision: 23, rangeMin: 127, rangeMax: 127 }),
      };
    }
    return {
      canvas: this,
      fillRect() {}, clearRect() {},
      getImageData: (x, y, width = 1, height = 1) => ({ data: new Uint8ClampedArray(width * height * 4) }),
      putImageData() {}, createImageData: (width = 1, height = 1) => ({ data: new Uint8ClampedArray(width * height * 4) }),
      setTransform() {}, transform() {}, drawImage() {}, save() {}, restore() {}, beginPath() {},
      moveTo() {}, lineTo() {}, bezierCurveTo() {}, quadraticCurveTo() {}, closePath() {}, clip() {},
      stroke() {}, fill() {}, arc() {}, rect() {}, ellipse() {}, translate() {}, scale() {}, rotate() {},
      fillText() {}, strokeText() {}, measureText: (text) => ({ width: String(text).length * 8 }),
      createLinearGradient: () => ({ addColorStop() {} }), createRadialGradient: () => ({ addColorStop() {} }),
      createPattern: () => ({}), isPointInPath: () => false,
      font: '10px sans-serif', textBaseline: 'alphabetic', textAlign: 'start', fillStyle: '#000',
      strokeStyle: '#000', globalAlpha: 1, lineWidth: 1, shadowBlur: 0, shadowColor: '',
    };
  };
  proto.toDataURL = () => 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==';
  proto.toBlob = (callback) => callback && callback(null);

  window.Worker = class {
    postMessage() {}
    terminate() {}
    addEventListener() {}
    removeEventListener() {}
    onmessage = null;
    onerror = null;
  };
  window.OffscreenCanvas = class {
    constructor(width, height) { this.width = width; this.height = height; }
    getContext(type) { return proto.getContext.call(this, type); }
  };

  try {
    Object.defineProperty(window.document, 'hidden', { value: false, configurable: true });
    Object.defineProperty(window.document, 'visibilityState', { value: 'visible', configurable: true });
  } catch {}
  const navigatorValues = {
    userAgent: FAKE_UA,
    platform: 'Win32',
    language: 'en-US',
    languages: ['en-US', 'en'],
    vendor: 'Google Inc.',
    webdriver: false,
    hardwareConcurrency: 8,
    deviceMemory: 8,
    maxTouchPoints: 0,
    cookieEnabled: true,
    plugins: { length: 3, item: () => null, namedItem: () => null, refresh() {} },
    mimeTypes: { length: 0, item: () => null, namedItem: () => null },
  };
  for (const [key, value] of Object.entries(navigatorValues)) {
    try { Object.defineProperty(window.navigator, key, { value, configurable: true }); } catch {}
  }
  try {
    Object.defineProperty(window, 'screen', {
      value: { width: 1920, height: 1080, availWidth: 1920, availHeight: 1040, colorDepth: 24, pixelDepth: 24 },
      configurable: true,
    });
  } catch {}
  window.chrome = { runtime: {} };
  window.outerWidth = 1920;
  window.outerHeight = 1080;
  window.innerWidth = 1280;
  window.innerHeight = 720;
  window.devicePixelRatio = 1;
  try {
    window.localStorage.setItem('__zcode_probe', '1');
    window.localStorage.removeItem('__zcode_probe');
  } catch {}
}

function waitFor(condition, timeout) {
  return new Promise((resolve, reject) => {
    const started = Date.now();
    const interval = setInterval(() => {
      let ready = false;
      try { ready = Boolean(condition()); } catch {}
      if (ready) {
        clearInterval(interval);
        resolve();
      } else if (Date.now() - started > timeout) {
        clearInterval(interval);
        reject(new Error(`SDK load timeout after ${timeout}ms`));
      }
    }, 80);
  });
}

async function solve() {
  let sdk;
  try {
    sdk = fs.readFileSync(SDK_PATH, 'utf8');
  } catch (error) {
    throw new Error(`local SDK unavailable: ${safeError(error)}`);
  }
  const sdkSafe = sdk.replace(/<\/script>/gi, '<\\/script>');
  const html = `<!DOCTYPE html><html><head></head><body><div id="cap"></div><button id="btn"></button><script>${sdkSafe}</script></body></html>`;
  const virtualConsole = new VirtualConsole();
  const dom = new JSDOM(html, {
    url: 'https://zcode.z.ai/',
    runScripts: 'dangerously',
    resources: new FeiLinBlockingLoader(),
    pretendToBeVisual: true,
    virtualConsole,
    beforeParse(window) {
      applyPolyfills(window);
      window.AliyunCaptchaConfig = { region: REGION, prefix: PREFIX };
    },
  });
  const { window } = dom;
  try {
    await waitFor(() => typeof window.initAliyunCaptcha === 'function', SDK_LOAD_TIMEOUT);
    return await new Promise((resolve, reject) => {
      const timeout = setTimeout(() => reject(new Error(`captcha solve timeout after ${SOLVE_TIMEOUT}ms`)), SOLVE_TIMEOUT);
      const rejectWith = (kind, error, exitCode) => {
        clearTimeout(timeout);
        reject(captchaError(kind, error, exitCode));
      };
      try {
        window.initAliyunCaptcha({
          SceneId: SCENE,
          mode: 'popup',
          region: REGION,
          prefix: PREFIX,
          language: 'en',
          element: '#cap',
          button: '#btn',
          captchaLogoImg: '',
          showErrorTip: false,
          getInstance(instance) {
            const start = instance && (instance.startTracelessVerification || instance.show);
            if (typeof start !== 'function') {
              rejectWith('SDK instance unavailable', null, 5);
              return;
            }
            try { start.call(instance); } catch (error) { rejectWith('SDK start error', error, 5); }
          },
          success(param) {
            clearTimeout(timeout);
            resolve(param);
          },
          fail(error) { rejectWith('SDK fail', error, 4); },
          onError(error) { rejectWith('SDK error', error, 5); },
        });
      } catch (error) {
        rejectWith('SDK init error', error, 5);
      }
    });
  } finally {
    try { window.close(); } catch {}
  }
}

(async () => {
  try {
    const param = await solve();
    if (typeof param !== 'string' || !param.trim()) throw new Error('SDK returned an empty result');
    fs.writeSync(1, `VERIFY_PARAM=${param.trim()}\n`);
    process.exit(0);
  } catch (error) {
    const exitCode = Number(error && error.exitCode) || 3;
    const marker = error && error.kind ? error.kind.replace(/[^A-Za-z ]/g, '').replace(/ +/g, '_').toUpperCase() : 'SOLVER_ERROR';
    fs.writeSync(2, `${marker}=${safeError(error)}\n`);
    process.exit(exitCode);
  }
})();
