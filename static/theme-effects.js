"use strict";

// Matrix code rain: a canvas behind the page, drawn only while the Matrix
// theme and animated effects are on. About 15 frames a second, paused when
// the page is hidden, and a single still frame for reduced-motion devices.
(() => {
  const glyphs = "アイウエオカキクケコサシスセソタチツテトナニヌネノハヒフヘホマミムメモヤユヨラリルレロワン0123456789:.=*+-<>¦|";
  const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)");
  let canvas = null;
  let context = null;
  let drops = [];
  let timer = 0;
  const size = 16;

  function active() {
    const root = document.documentElement;
    return root.dataset.theme === "matrix";
  }

  function animated() {
    return document.documentElement.dataset.effects === "on" && !reducedMotion.matches && !document.hidden;
  }

  function resize() {
    if (!canvas) return;
    const ratio = Math.min(2, window.devicePixelRatio || 1);
    canvas.width = Math.floor(window.innerWidth * ratio);
    canvas.height = Math.floor(window.innerHeight * ratio);
    context.setTransform(ratio, 0, 0, ratio, 0, 0);
    const columns = Math.ceil(window.innerWidth / size);
    drops = Array.from({ length: columns }, (_, index) => drops[index] ?? Math.random() * -50);
    context.fillStyle = "#000";
    context.fillRect(0, 0, window.innerWidth, window.innerHeight);
  }

  function frame() {
    context.fillStyle = "rgb(0 0 0 / 0.12)";
    context.fillRect(0, 0, window.innerWidth, window.innerHeight);
    context.font = `${size}px ui-monospace, Menlo, Consolas, monospace`;
    for (let column = 0; column < drops.length; column++) {
      const y = drops[column] * size;
      if (y > 0) {
        const glyph = glyphs[Math.floor(Math.random() * glyphs.length)];
        context.fillStyle = Math.random() < 0.06 ? "#d4ffd9" : "#00ff41";
        context.fillText(glyph, column * size, y);
      }
      drops[column] = y > window.innerHeight && Math.random() > 0.975 ? 0 : drops[column] + 1;
    }
  }

  function stop() {
    window.clearTimeout(timer);
    timer = 0;
  }

  function loop() {
    frame();
    timer = window.setTimeout(loop, 66);
  }

  function update() {
    stop();
    if (!active()) {
      canvas?.remove();
      canvas = null;
      return;
    }
    if (!canvas) {
      canvas = document.createElement("canvas");
      canvas.id = "theme-rain";
      canvas.setAttribute("aria-hidden", "true");
      context = canvas.getContext("2d");
      document.body.prepend(canvas);
      resize();
    }
    if (animated()) loop();
    else for (let step = 0; step < 60; step++) frame(); // a settled still frame
  }

  window.addEventListener("monitor-theme-change", update);
  document.addEventListener("visibilitychange", update);
  reducedMotion.addEventListener?.("change", update);
  window.addEventListener("resize", () => { if (canvas) { resize(); update(); } });
  update();
})();
