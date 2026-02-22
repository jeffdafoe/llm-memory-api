const express = require('express');
const auth = require('./middleware/auth');
const opportunisticHeartbeat = require('./middleware/heartbeat');
const chatRoutes = require('./routes/chat');
const ingestRoutes = require('./routes/ingest');
const searchRoutes = require('./routes/search');
const deleteRoutes = require('./routes/delete');
const mailRoutes = require('./routes/mail');
const registerRoutes = require('./routes/register');
const presenceRoutes = require('./routes/presence');
const discussionRoutes = require('./routes/discussion');

const app = express();
const port = process.env.PORT || 3100;

app.use(express.json({ limit: '5mb' }));
app.use('/v1', auth);
app.use('/v1', opportunisticHeartbeat);
app.use('/v1', chatRoutes);
app.use('/v1', ingestRoutes);
app.use('/v1', searchRoutes);
app.use('/v1', deleteRoutes);
app.use('/v1', mailRoutes);
app.use('/v1', registerRoutes);
app.use('/v1', presenceRoutes);
app.use('/v1', discussionRoutes);

app.get('/health', (req, res) => {
    res.json({ status: 'ok' });
});

app.listen(port, () => {
    console.log(`Memory API listening on port ${port}`);
});
