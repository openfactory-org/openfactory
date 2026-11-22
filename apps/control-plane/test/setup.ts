// Setup of the control-plane test project: loads the worker module graph
// before the first test of each file, so that the first test does not pay for
// the cold module load.
import "../src/index";
