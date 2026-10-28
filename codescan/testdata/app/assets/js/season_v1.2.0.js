// The season page.
import('./util.js?v=1').then(u => u.init());
const id = 3;
fetch('/season/' + id);            // GET /season/{id}
const xhr = new XMLHttpRequest();
xhr.open('POST', `/admin/reset`);  // POST /admin/reset
fetch('/about/' + id);             // no such route
fetch('/assets/img/logo.png');     // a file, not a route
loadBeat('season-extra');          // a sibling script by name
importWithRetry('/assets/js/late_v1.2.0.js?v=' + id); // an absolute path, versioned
// fetch('/show') is commented out
